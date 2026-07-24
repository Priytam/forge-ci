package compiler

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// stringOrSlice accepts either a scalar or a sequence in YAML and always yields
// a []string. It powers extends: (one base or many) and could serve any
// "one-or-list" key.
type stringOrSlice []string

func (s *stringOrSlice) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		var one string
		if err := n.Decode(&one); err != nil {
			return err
		}
		*s = []string{one}
	case yaml.SequenceNode:
		var many []string
		if err := n.Decode(&many); err != nil {
			return err
		}
		*s = many
	default:
		return fmt.Errorf("expected a string or list of strings")
	}
	return nil
}

// mergeSpec deep-merges a parent spec with a child overlay and returns the
// result. The child wins on every key it sets; unset child keys inherit the
// parent. This is the single merge rule shared by extends: (parent = base job)
// and include: (parent = included fragment, child = main config job).
//
// Merge rules per field:
//   - scalars (stage/image/environment/timeout/when): child non-empty overrides
//   - pointers (retry/allowFailure/parallel): child non-nil overrides
//   - slices (script/needs/only/except/tags/rules/artifact paths): child
//     non-nil replaces (GitLab replaces arrays; it does not element-merge them)
//   - variables (map): union-merged, child key wins (GitLab deep-merges hashes)
//
// extends is intentionally NOT inherited — it is the directive being resolved.
func mergeSpec(parent, child jobSpec) jobSpec {
	out := parent

	if child.Stage != "" {
		out.Stage = child.Stage
	}
	if child.Image != "" {
		out.Image = child.Image
	}
	if child.Environment != "" {
		out.Environment = child.Environment
	}
	if child.Timeout != "" {
		out.Timeout = child.Timeout
	}
	if child.When != "" {
		out.When = child.When
	}
	if child.Retry != nil {
		out.Retry = child.Retry
	}
	if child.AllowFailure != nil {
		out.AllowFailure = child.AllowFailure
	}
	if child.Parallel != nil {
		out.Parallel = child.Parallel
	}
	if child.Script != nil {
		out.Script = child.Script
	}
	if child.Needs != nil {
		out.Needs = child.Needs
	}
	if child.Only != nil {
		out.Only = child.Only
	}
	if child.Except != nil {
		out.Except = child.Except
	}
	if child.Tags != nil {
		out.Tags = child.Tags
	}
	if child.Rules != nil {
		out.Rules = child.Rules
	}
	if child.Artifacts.Paths != nil {
		out.Artifacts.Paths = child.Artifacts.Paths
	}

	// Variables: union, child wins per key.
	if len(parent.Variables) > 0 || len(child.Variables) > 0 {
		merged := map[string]string{}
		for k, v := range parent.Variables {
			merged[k] = v
		}
		for k, v := range child.Variables {
			merged[k] = v
		}
		out.Variables = merged
	}

	// The result no longer carries the child's extends directive.
	out.Extends = nil
	return out
}

// resolveExtends expands every job's extends: chain into a fully-merged spec.
// Bases may themselves extend (chains), may be hidden (leading-dot) template
// jobs, and a job may list multiple bases (merged left-to-right, then the child
// on top). Cycles are detected and reported.
func resolveExtends(jobs map[string]jobSpec) (map[string]jobSpec, error) {
	resolved := map[string]jobSpec{}
	visiting := map[string]bool{}

	var resolve func(name string) (jobSpec, error)
	resolve = func(name string) (jobSpec, error) {
		if r, ok := resolved[name]; ok {
			return r, nil
		}
		spec, ok := jobs[name]
		if !ok {
			return jobSpec{}, fmt.Errorf("extends references unknown job %q", name)
		}
		if len(spec.Extends) == 0 {
			resolved[name] = spec
			return spec, nil
		}
		if visiting[name] {
			return jobSpec{}, fmt.Errorf("extends cycle detected at %q", name)
		}
		visiting[name] = true
		defer delete(visiting, name)

		base := jobSpec{}
		for _, parent := range spec.Extends {
			pr, err := resolve(parent)
			if err != nil {
				return jobSpec{}, fmt.Errorf("job %q: %w", name, err)
			}
			base = mergeSpec(base, pr)
		}
		merged := mergeSpec(base, spec)
		resolved[name] = merged
		return merged, nil
	}

	for name := range jobs {
		if _, err := resolve(name); err != nil {
			return nil, err
		}
	}
	return resolved, nil
}
