package compiler

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// TemplateFunc resolves a named template to its YAML body. It returns
// (yaml, true, nil) when found, ("", false, nil) when there is no such
// template, and a non-nil error only on a lookup failure. A nil TemplateFunc
// means "no templates are available"; any include that names one then errors.
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
		if tmpl == nil {
			return config{}, fmt.Errorf("include: template %q requested but no template store is available", inc.Template)
		}
		body, found, err := tmpl(inc.Template)
		if err != nil {
			return config{}, fmt.Errorf("include: loading template %q: %w", inc.Template, err)
		}
		if !found {
			return config{}, fmt.Errorf("include: template %q is not registered for this repo", inc.Template)
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
	return out
}
