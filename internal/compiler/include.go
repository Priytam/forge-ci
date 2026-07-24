package compiler

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// TemplateFunc resolves a named template to its YAML body. It returns
// (yaml, true, nil) when found, ("", false, nil) when there is no such
// template, and a non-nil error only on a lookup failure. A nil TemplateFunc
// means "no repo template store is available" — includes then resolve against
// the shipped built-in templates only (see builtins.go and resolveTemplateBody).
//
// Forge does not host repo file trees and webhooks carry no file contents, so
// there is nothing to resolve a filesystem-style `include: local:` against.
// Instead templates are registered per repo (repo_templates table) and referred
// to by name — see docs/pipeline-dsl.md. This is the honest design for Forge;
// remote/URL includes are intentionally NOT supported (no network fetch at
// compile time).
type TemplateFunc func(name string) (yaml string, found bool, err error)

// includeSpec is one entry of the top-level include: list. Only the template
// form is supported.
type includeSpec struct {
	Template string `yaml:"template"`
}

// resolveTemplateBody resolves a template name to its YAML body, checking the
// per-repo template store FIRST (the caller-supplied tmpl) and falling back to
// a shipped built-in (see builtins.go) only when the store has no template by
// that name. This gives repos the ability to shadow any built-in by
// registering a template of the same name, while keeping built-ins available
// even when tmpl is nil (no store at all — e.g. a nil-resolver Compile).
//
// Returns (body, true, nil) when resolved, ("", false, nil) when neither the
// store nor the built-ins have it, and a non-nil error only on a store lookup
// failure.
func resolveTemplateBody(name string, tmpl TemplateFunc) (string, bool, error) {
	if tmpl != nil {
		body, found, err := tmpl(name)
		if err != nil {
			return "", false, err
		}
		if found {
			return body, true, nil
		}
	}
	if body, ok := BuiltinTemplate(name); ok {
		return body, true, nil
	}
	return "", false, nil
}

// resolveIncludes fetches every included template, parses each as a config
// fragment, and merges them (in listed order) UNDER the main config: included
// fragments seed stages/jobs/default, then each later include overrides earlier
// ones, and finally the main config overrides all includes on any key conflict.
// Jobs are deep-merged by name; stage lists are unioned preserving order.
//
// Include recursion (an included template that itself has include:) is
// supported up to a small depth bound to prevent runaway/cyclic includes.
func resolveIncludes(main config, tmpl TemplateFunc) (config, error) {
	return resolveIncludesDepth(main, tmpl, 0)
}

const maxIncludeDepth = 10

func resolveIncludesDepth(main config, tmpl TemplateFunc, depth int) (config, error) {
	if len(main.Include) == 0 {
		return main, nil
	}
	if depth > maxIncludeDepth {
		return config{}, fmt.Errorf("include: nesting too deep (possible include cycle)")
	}

	// Accumulate merged fragments, in order, starting from empty.
	merged := config{}
	for _, inc := range main.Include {
		if inc.Template == "" {
			return config{}, fmt.Errorf("include: only the {template: <name>} form is supported")
		}
		body, found, err := resolveTemplateBody(inc.Template, tmpl)
		if err != nil {
			return config{}, fmt.Errorf("include: loading template %q: %w", inc.Template, err)
		}
		if !found {
			return config{}, fmt.Errorf("include: template %q is not registered for this repo and is not a built-in", inc.Template)
		}
		var frag config
		if err := yaml.Unmarshal([]byte(body), &frag); err != nil {
			return config{}, fmt.Errorf("include: template %q has invalid YAML: %w", inc.Template, err)
		}
		// Resolve the fragment's own includes first (depth-bounded).
		frag, err = resolveIncludesDepth(frag, tmpl, depth+1)
		if err != nil {
			return config{}, err
		}
		merged = mergeConfig(merged, frag)
	}

	// Main overrides all included fragments.
	mainNoInclude := main
	mainNoInclude.Include = nil
	return mergeConfig(merged, mainNoInclude), nil
}

// mergeConfig merges an overlay config over a base: jobs are deep-merged by
// name (overlay wins per key via mergeSpec), stage lists are unioned preserving
// order, default/auto_cancel are taken from the overlay when it sets them.
func mergeConfig(base, overlay config) config {
	out := config{}

	// Stages: union, base order first, then any new overlay stages.
	seen := map[string]bool{}
	for _, s := range base.Stages {
		if !seen[s] {
			out.Stages = append(out.Stages, s)
			seen[s] = true
		}
	}
	for _, s := range overlay.Stages {
		if !seen[s] {
			out.Stages = append(out.Stages, s)
			seen[s] = true
		}
	}

	// Jobs: deep-merge by name.
	out.Jobs = map[string]jobSpec{}
	for name, spec := range base.Jobs {
		out.Jobs[name] = spec
	}
	for name, spec := range overlay.Jobs {
		if existing, ok := out.Jobs[name]; ok {
			out.Jobs[name] = mergeSpec(existing, spec)
		} else {
			out.Jobs[name] = spec
		}
	}

	// default: overlay wins per subfield.
	out.Default = base.Default
	if overlay.Default.Timeout != "" {
		out.Default.Timeout = overlay.Default.Timeout
	}
	if overlay.Default.Retry != nil {
		out.Default.Retry = overlay.Default.Retry
	}

	// auto_cancel: overlay wins when set.
	out.AutoCancel = base.AutoCancel
	if overlay.AutoCancel != nil {
		out.AutoCancel = overlay.AutoCancel
	}

	// fail_fast: overlay wins when set.
	out.FailFast = base.FailFast
	if overlay.FailFast != nil {
		out.FailFast = overlay.FailFast
	}
	return out
}
