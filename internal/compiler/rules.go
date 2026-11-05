package compiler

import "fmt"

// ruleSpec is one entry in a job's rules: list. Clauses combine with AND: a
// rule matches when every clause it declares is satisfied. The first matching
// rule decides the job's fate (inclusion + when + overrides); evaluation stops
// there.
type ruleSpec struct {
	If           string            `yaml:"if"`
	Changes      []string          `yaml:"changes"`
	Exists       []string          `yaml:"exists"`
	When         string            `yaml:"when"`
	AllowFailure *bool             `yaml:"allow_failure"`
	Variables    map[string]string `yaml:"variables"`
}

// ruleOutcome is the effect of the first matching rule.
type ruleOutcome struct {
	included     bool
	when         string            // on_success | manual | always (never => excluded)
	allowFailure *bool             // rule-level override, if set
	variables    map[string]string // rule-level variables to inject, if any
}

var validWhen = map[string]bool{
	"on_success": true,
	"manual":     true,
	"always":     true,
	"never":      true,
}

// evalRules walks a job's rules in order and returns the outcome of the first
// matching rule. If no rule matches, the job is excluded (GitLab semantics).
//
// changes: is satisfied when any file in the pipeline's change set matches
// one of its globs; with an unknown change set (nil) it is satisfied, so jobs
// run rather than being skipped on a guess. exists: remains a documented
// no-op (the compiler has no checkout) and is always satisfied.
func evalRules(rules []ruleSpec, ctx map[string]string, changes *ChangeSet) (ruleOutcome, error) {
	for i, rule := range rules {
		if rule.When != "" && !validWhen[rule.When] {
			return ruleOutcome{}, fmt.Errorf("rules[%d]: invalid when %q (want on_success|manual|always|never)", i, rule.When)
		}
		match := true
		if rule.If != "" {
			ok, err := evalExpr(rule.If, ctx)
			if err != nil {
				return ruleOutcome{}, fmt.Errorf("rules[%d]: %w", i, err)
			}
			match = ok
		}
		if match && len(rule.Changes) > 0 {
			match = changes.matchesChanges(rule.Changes)
		}
		// exists: satisfied (no-op) — documented limitation.
		if !match {
			continue
		}
		when := rule.When
		if when == "" {
			when = "on_success"
		}
		if when == "never" {
			return ruleOutcome{included: false}, nil
		}
		return ruleOutcome{
			included:     true,
			when:         when,
			allowFailure: rule.AllowFailure,
			variables:    rule.Variables,
		}, nil
	}
	// No rule matched -> job not added.
	return ruleOutcome{included: false}, nil
}
