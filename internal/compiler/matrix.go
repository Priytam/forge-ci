package compiler

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// parallelSpec models both forms of the parallel: key:
//
//	parallel: 5                       # run 5 identical instances
//	parallel:
//	  matrix:
//	    - VAR: [a, b]
//	      OTHER: [x, y]               # cartesian product -> one job per combo
//
// A matrix is a LIST of hashes; within each hash the values are cartesian-
// multiplied, and the per-hash products are concatenated (GitLab semantics).
type parallelSpec struct {
	count  int                   // parallel: N  (0 when matrix form)
	matrix []map[string][]string // ordered list of matrix hashes
	// keyOrder preserves the YAML key order per matrix hash so generated job
	// names and CI_NODE-style expansion are stable across runs.
	keyOrder [][]string
}

func (ps *parallelSpec) UnmarshalYAML(n *yaml.Node) error {
	// Scalar form: parallel: N
	if n.Kind == yaml.ScalarNode {
		var count int
		if err := n.Decode(&count); err != nil {
			return fmt.Errorf("parallel: must be an integer or a {matrix: ...} mapping")
		}
		ps.count = count
		return nil
	}
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("parallel: must be an integer or a {matrix: ...} mapping")
	}

	// Mapping form: parallel: { matrix: [ ... ] }
	var raw struct {
		Matrix []yaml.Node `yaml:"matrix"`
	}
	if err := n.Decode(&raw); err != nil {
		return err
	}
	if len(raw.Matrix) == 0 {
		return fmt.Errorf("parallel.matrix: must be a non-empty list of variable maps")
	}
	for _, entry := range raw.Matrix {
		if entry.Kind != yaml.MappingNode {
			return fmt.Errorf("parallel.matrix: each entry must be a mapping of VAR to a list of values")
		}
		dims := map[string][]string{}
		var order []string
		// entry.Content is [key1, val1, key2, val2, ...]
		for i := 0; i+1 < len(entry.Content); i += 2 {
			key := entry.Content[i].Value
			valNode := entry.Content[i+1]
			var vals []string
			switch valNode.Kind {
			case yaml.SequenceNode:
				if err := valNode.Decode(&vals); err != nil {
					return fmt.Errorf("parallel.matrix: values for %q must be scalars", key)
				}
			case yaml.ScalarNode:
				vals = []string{valNode.Value}
			default:
				return fmt.Errorf("parallel.matrix: values for %q must be a scalar or list", key)
			}
			if len(vals) == 0 {
				return fmt.Errorf("parallel.matrix: %q must list at least one value", key)
			}
			dims[key] = vals
			order = append(order, key)
		}
		if len(order) == 0 {
			return fmt.Errorf("parallel.matrix: each entry must set at least one variable")
		}
		ps.matrix = append(ps.matrix, dims)
		ps.keyOrder = append(ps.keyOrder, order)
	}
	return nil
}

// expansion is one concrete instance produced from a job's parallel/matrix.
type expansion struct {
	nameSuffix string            // appended to the base job name
	vars       map[string]string // variables injected into this instance
}

// expand returns the list of instances a parallel/matrix produces. A nil or
// empty parallelSpec yields a single instance with no suffix and no extra vars
// (i.e. the job is unchanged).
func (ps *parallelSpec) expand() ([]expansion, error) {
	if ps == nil || (ps.count == 0 && len(ps.matrix) == 0) {
		return []expansion{{}}, nil
	}

	// parallel: N
	if ps.count > 0 {
		if ps.count < 1 || ps.count > maxParallel {
			return nil, fmt.Errorf("parallel: must be between 1 and %d", maxParallel)
		}
		out := make([]expansion, 0, ps.count)
		for i := 1; i <= ps.count; i++ {
			out = append(out, expansion{
				nameSuffix: fmt.Sprintf(" %d/%d", i, ps.count),
				vars: map[string]string{
					"CI_NODE_INDEX": strconv.Itoa(i),
					"CI_NODE_TOTAL": strconv.Itoa(ps.count),
				},
			})
		}
		return out, nil
	}

	// parallel: { matrix: [...] }
	var out []expansion
	for hi, dims := range ps.matrix {
		order := ps.keyOrder[hi]
		combos := cartesian(dims, order)
		if len(combos) > maxParallel {
			return nil, fmt.Errorf("parallel.matrix expands to %d jobs, exceeding the limit of %d", len(combos), maxParallel)
		}
		for _, combo := range combos {
			// Name suffix uses key-sorted values for a stable, unique name,
			// e.g. "deploy: [linux, amd64]".
			keys := append([]string(nil), order...)
			sort.Strings(keys)
			parts := make([]string, 0, len(keys))
			for _, k := range keys {
				parts = append(parts, combo[k])
			}
			out = append(out, expansion{
				nameSuffix: fmt.Sprintf(": [%s]", strings.Join(parts, ", ")),
				vars:       combo,
			})
		}
	}
	// Guard against two matrix hashes producing colliding names.
	seen := map[string]bool{}
	for _, e := range out {
		if seen[e.nameSuffix] {
			return nil, fmt.Errorf("parallel.matrix produces duplicate combination %q", strings.TrimPrefix(e.nameSuffix, ": "))
		}
		seen[e.nameSuffix] = true
	}
	return out, nil
}

// cartesian returns every combination of the dimensions, iterating keys in the
// given order so output is deterministic.
func cartesian(dims map[string][]string, order []string) []map[string]string {
	result := []map[string]string{{}}
	for _, key := range order {
		values := dims[key]
		var next []map[string]string
		for _, partial := range result {
			for _, v := range values {
				combo := make(map[string]string, len(partial)+1)
				for k, pv := range partial {
					combo[k] = pv
				}
				combo[key] = v
				next = append(next, combo)
			}
		}
		result = next
	}
	return result
}
