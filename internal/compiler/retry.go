package compiler

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// retrySpec is retry: as a number (retry on any failure up to N times) or
// the long form:
//
//	retry:
//	  max: 1
//	  when: [runner_system_failure]   # script_failure | runner_system_failure | always
type retrySpec struct {
	Max  *int     `yaml:"max"`
	When []string `yaml:"when"`
}

func (r *retrySpec) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		var v int
		if err := n.Decode(&v); err != nil {
			return fmt.Errorf("retry must be a number or {max, when}")
		}
		r.Max = &v
		return nil
	}
	type plain retrySpec
	var p plain
	if err := n.Decode(&p); err != nil {
		return err
	}
	if p.Max == nil {
		return fmt.Errorf("retry: max is required in the long form")
	}
	*r = retrySpec(p)
	return nil
}

// Retry conditions.
const (
	RetryAlways              = "always"
	RetryScriptFailure       = "script_failure"
	RetryRunnerSystemFailure = "runner_system_failure"
)

var retryWhenValues = map[string]bool{RetryAlways: true, RetryScriptFailure: true, RetryRunnerSystemFailure: true}

// validateRetryWhen normalises a when: list (empty → [always]).
func validateRetryWhen(where string, when []string) ([]string, error) {
	if len(when) == 0 {
		return []string{RetryAlways}, nil
	}
	out := []string{}
	seen := map[string]bool{}
	for _, w := range when {
		w = strings.TrimSpace(w)
		if !retryWhenValues[w] {
			return nil, fmt.Errorf("%s: retry.when %q must be one of always, script_failure, runner_system_failure", where, w)
		}
		if !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	sort.Strings(out)
	return out, nil
}

// RetryAppliesTo reports whether a when: list covers a failure kind.
func RetryAppliesTo(when []string, kind string) bool {
	if len(when) == 0 {
		return true
	}
	for _, w := range when {
		if w == RetryAlways || w == kind {
			return true
		}
	}
	return false
}
