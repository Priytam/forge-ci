package compiler

import (
	"fmt"
	"strings"

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
// to by name — see docs/pipeline-dsl.md. Includes from other repos, URLs and
// the template catalog go through an IncludeFunc supplied by the server (the
// compiler itself never touches the network).
type TemplateFunc func(name string) (yaml string, found bool, err error)

// includeSpec is one entry of the top-level include: list:
//
//   - template: security/sast          # repo template or built-in by name
//   - template: deploy/k8s@1.4.0        # template catalog, pinned version
//   - project: platform/ci-templates    # a file from another connected repo
//     ref: v2.1.0                       # tag, branch or commit (pin it)
//     file: deploy.yml
//   - remote: https://example.com/ci/lint.yml
//     integrity: sha256-<base64 digest>  # optional pin of the exact content
//
// Any form may pass inputs: to a template that declares spec: inputs:.
type includeSpec struct {
	Template  string         `yaml:"template"`
	Project   string         `yaml:"project"`
	Ref       string         `yaml:"ref"`
	File      string         `yaml:"file"`
	Remote    string         `yaml:"remote"`
	Integrity string         `yaml:"integrity"`
	Inputs    map[string]any `yaml:"inputs"`
}

// IncludeRef is an include that needs the outside world (another repo, a URL
// or the template catalog), handed to an IncludeFunc.
type IncludeRef struct {
	Kind      string // "project" | "remote" | "catalog"
	Project   string
	Ref       string
	File      string
	URL       string
	Integrity string
	Name      string // catalog template name
	Version   string // catalog version ("" or "latest" = newest)
}

// IncludeFunc resolves an external include. found=false means it doesn't
// exist; err is reserved for lookup failures (network, auth, integrity).
type IncludeFunc func(ref IncludeRef) (yaml string, found bool, err error)

// describe names an include for error messages.
func (inc includeSpec) describe() string {
	switch {
	case inc.Project != "":
		return fmt.Sprintf("project %s@%s:%s", inc.Project, inc.Ref, inc.File)
	case inc.Remote != "":
		return "remote " + inc.Remote
	default:
		return "template " + inc.Template
	}
}

// loadInclude returns the YAML body of one include.
func loadInclude(inc includeSpec, tmpl TemplateFunc, ext IncludeFunc) (string, error) {
	set := 0
	for _, v := range []string{inc.Template, inc.Project, inc.Remote} {
		if v != "" {
			set++
		}
	}
	if set != 1 {
		return "", fmt.Errorf("include: each entry needs exactly one of template:, project: or remote:")
	}
	external := func(ref IncludeRef) (string, error) {
		if ext == nil {
			return "", fmt.Errorf("include: %s is not available here (no external include resolver)", inc.describe())
		}
		body, found, err := ext(ref)
		if err != nil {
			return "", fmt.Errorf("include: loading %s: %w", inc.describe(), err)
		}
		if !found {
			return "", fmt.Errorf("include: %s not found", inc.describe())
		}
		return body, nil
	}
	switch {
	case inc.Project != "":
		if inc.File == "" {
			return "", fmt.Errorf("include: project %s needs file:", inc.Project)
		}
		return external(IncludeRef{Kind: "project", Project: inc.Project, Ref: inc.Ref, File: inc.File})
	case inc.Remote != "":
		return external(IncludeRef{Kind: "remote", URL: inc.Remote, Integrity: inc.Integrity})
	}
	name, version, versioned := strings.Cut(inc.Template, "@")
	if versioned {
		return external(IncludeRef{Kind: "catalog", Name: name, Version: version})
	}
	body, found, err := resolveTemplateBody(inc.Template, tmpl)
	if err != nil {
		return "", fmt.Errorf("include: loading template %q: %w", inc.Template, err)
	}
	if found {
		return body, nil
	}
	// Not a repo template or built-in: the catalog's newest version, if any.
	if ext != nil {
		if b, ok, err := ext(IncludeRef{Kind: "catalog", Name: inc.Template}); err == nil && ok {
			return b, nil
		}
	}
	return "", fmt.Errorf("include: template %q is not registered for this repo and is not a built-in", inc.Template)
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
func resolveIncludes(main config, tmpl TemplateFunc, ext IncludeFunc) (config, error) {
	return resolveIncludesDepth(main, tmpl, ext, 0)
}

const maxIncludeDepth = 10

func resolveIncludesDepth(main config, tmpl TemplateFunc, ext IncludeFunc, depth int) (config, error) {
	if len(main.Include) == 0 {
		return main, nil
	}
	if depth > maxIncludeDepth {
		return config{}, fmt.Errorf("include: nesting too deep (possible include cycle)")
	}

	// Accumulate merged fragments, in order, starting from empty.
	merged := config{}
	for _, inc := range main.Include {
		raw, err := loadInclude(inc, tmpl, ext)
		if err != nil {
			return config{}, err
		}
		body, err := applyInputs(raw, inc.Inputs)
		if err != nil {
			return config{}, fmt.Errorf("include: %s: %w", inc.describe(), err)
		}
		var frag config
		if err := yaml.Unmarshal([]byte(body), &frag); err != nil {
			return config{}, fmt.Errorf("include: %s has invalid YAML: %w", inc.describe(), err)
		}
		// Resolve the fragment's own includes first (depth-bounded).
		frag, err = resolveIncludesDepth(frag, tmpl, ext, depth+1)
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
// name (overlay wins per key via mergeSpec), stage lists are unioned (see
// mergeStages), default/auto_cancel are taken from the overlay when it sets them.
func mergeConfig(base, overlay config) config {
	out := config{}
	out.Stages = mergeStages(base.Stages, overlay.Stages)

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
	if overlay.Default.Retry.Max != nil {
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

// mergeStages unions two stage lists. Stages the overlay does not mention keep
// their base order and come first; everything the overlay lists follows in the
// OVERLAY's order.
//
// The overlay is the main config (includes are merged underneath it), so this is
// what lets an explicit `stages:` mean what it says. Previously the base order
// was emitted wholesale first, so a config declaring
//
//	stages: [lint, test, build, deploy]
//
// that included a built-in (each declares `stages: [test]`) rendered as
//
//	[test, lint, build, deploy]
//
// — the included `test` outranking a `lint` the author had put before it.
//
// Stages that appear ONLY in an include still sort ahead of main-only stages,
// which is what makes an unpositioned scan run security-first; declaring the
// stage in the main config is now the way to place it.
func mergeStages(base, overlay []string) []string {
	inOverlay := make(map[string]bool, len(overlay))
	for _, s := range overlay {
		inOverlay[s] = true
	}
	out := make([]string, 0, len(base)+len(overlay))
	seen := map[string]bool{}
	// Base-only stages, in base order.
	for _, s := range base {
		if !inOverlay[s] && !seen[s] {
			out = append(out, s)
			seen[s] = true
		}
	}
	// Then the overlay's own order, which positions anything it names.
	for _, s := range overlay {
		if !seen[s] {
			out = append(out, s)
			seen[s] = true
		}
	}
	return out
}
