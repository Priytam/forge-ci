package compiler

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Template inputs (GitLab-style). A template may start with a spec header
// document declaring its inputs, then "---", then the body, which refers to
// them as $[[ inputs.name ]]:
//
//	spec:
//	  inputs:
//	    environment: {default: staging, options: [staging, production]}
//	    replicas: {default: "2"}
//	    cluster: {}                    # required: no default
//	---
//	jobs:
//	  deploy:
//	    stage: deploy
//	    script: [./deploy.sh $[[ inputs.environment ]] --replicas $[[ inputs.replicas ]] --cluster $[[ inputs.cluster ]]]
//
// The include passes values with inputs:. Unknown inputs, missing required
// inputs and values outside options are errors.

type inputSpec struct {
	Default     any      `yaml:"default"`
	Description string   `yaml:"description"`
	Options     []string `yaml:"options"`
}

var inputRefRe = regexp.MustCompile(`\$\[\[\s*inputs\.([A-Za-z0-9_-]+)\s*\]\]`)

// splitSpec separates a leading spec document from the template body.
func splitSpec(body string) (spec map[string]inputSpec, rest string, hasSpec bool, err error) {
	trimmed := strings.TrimLeft(body, "\n")
	if !strings.HasPrefix(trimmed, "spec:") {
		return nil, body, false, nil
	}
	head, tail, found := strings.Cut(trimmed, "\n---")
	if !found {
		return nil, body, false, fmt.Errorf("template has a spec: header but no --- separating it from the body")
	}
	var doc struct {
		Spec struct {
			Inputs map[string]inputSpec `yaml:"inputs"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal([]byte(head), &doc); err != nil {
		return nil, body, true, fmt.Errorf("invalid spec header: %w", err)
	}
	// Drop the rest of the separator line.
	if i := strings.Index(tail, "\n"); i >= 0 {
		tail = tail[i+1:]
	} else {
		tail = ""
	}
	return doc.Spec.Inputs, tail, true, nil
}

// applyInputs validates inputs against the template's spec and substitutes
// them into the body. A template without a spec header takes no inputs.
func applyInputs(body string, given map[string]any) (string, error) {
	spec, rest, hasSpec, err := splitSpec(body)
	if err != nil {
		return "", err
	}
	if !hasSpec {
		if len(given) > 0 {
			return "", fmt.Errorf("inputs given, but the template declares no spec: inputs:")
		}
		return body, nil
	}
	values := map[string]string{}
	for name, v := range given {
		if _, ok := spec[name]; !ok {
			known := make([]string, 0, len(spec))
			for k := range spec {
				known = append(known, k)
			}
			sort.Strings(known)
			return "", fmt.Errorf("unknown input %q (the template accepts: %s)", name, strings.Join(known, ", "))
		}
		values[name] = fmt.Sprint(v)
	}
	for name, s := range spec {
		v, ok := values[name]
		if !ok {
			if s.Default == nil {
				return "", fmt.Errorf("input %q is required", name)
			}
			v = fmt.Sprint(s.Default)
			values[name] = v
		}
		if len(s.Options) > 0 && !contains(s.Options, v) {
			return "", fmt.Errorf("input %q = %q is not one of %s", name, v, strings.Join(s.Options, ", "))
		}
	}
	var missing error
	out := inputRefRe.ReplaceAllStringFunc(rest, func(m string) string {
		name := inputRefRe.FindStringSubmatch(m)[1]
		v, ok := values[name]
		if !ok && missing == nil {
			missing = fmt.Errorf("template uses undeclared input %q", name)
		}
		return v
	})
	return out, missing
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// TemplateInput describes one declared input, for catalogs and docs.
type TemplateInput struct {
	Name        string
	Default     string
	Required    bool
	Description string
	Options     []string
}

// TemplateInputs lists a template's declared inputs (nil when it has none or
// its header doesn't parse).
func TemplateInputs(body string) []TemplateInput {
	spec, _, hasSpec, err := splitSpec(body)
	if err != nil || !hasSpec {
		return nil
	}
	out := make([]TemplateInput, 0, len(spec))
	for name, s := range spec {
		in := TemplateInput{Name: name, Required: s.Default == nil, Description: s.Description, Options: s.Options}
		if s.Default != nil {
			in.Default = fmt.Sprint(s.Default)
		}
		out = append(out, in)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ValidateTemplate checks that a template's spec header (if any) parses and
// that its body is valid config YAML once inputs are filled with their
// defaults (or the first option, or a placeholder for required ones).
func ValidateTemplate(body string) error {
	spec, _, hasSpec, err := splitSpec(body)
	if err != nil {
		return err
	}
	given := map[string]any{}
	if hasSpec {
		for name, s := range spec {
			switch {
			case s.Default != nil:
			case len(s.Options) > 0:
				given[name] = s.Options[0]
			default:
				given[name] = "placeholder"
			}
		}
	}
	filled, err := applyInputs(body, given)
	if err != nil {
		return err
	}
	var c config
	if err := yaml.Unmarshal([]byte(filled), &c); err != nil {
		return fmt.Errorf("invalid YAML: %w", err)
	}
	return nil
}
