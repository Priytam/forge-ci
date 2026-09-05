package compiler

import "sort"

// Built-in pipeline templates.
//
// These are named YAML fragments shipped with Forge so a repo can get common
// security scanning with a single include: line and zero setup — no template
// needs to be registered in the repo_templates table first. They resolve
// through the SAME include: [{template: name}] mechanism as per-repo templates
// (see include.go), and are merged with the exact same semantics (stages are
// unioned; jobs are deep-merged by name; the main config overrides includes).
//
// Precedence: a per-repo template (repo_templates) ALWAYS wins over a built-in
// of the same name. Resolution tries the repo store first and only falls back
// to a built-in when the store has no template by that name (see
// resolveTemplateBody in include.go). This means built-ins are available even
// for repos with zero registered templates, and even when Compile is called
// with a nil TemplateFunc (no store at all) — but a repo can still shadow any
// built-in by registering its own template under the same name.
//
// Each scan job lands in a `test` stage and defaults to allow_failure: true so
// a finding surfaces in the pipeline without hard-blocking it. A `test` stage
// the main config does not declare is unioned ahead of main-only stages (e.g.
// build/deploy), so scans run first ("security-first"); a main config that DOES
// declare `test` in its `stages:` positions it itself (see mergeStages in
// include.go). See docs/pipeline-dsl.md for how to make a scan blocking
// (override allow_failure) or point a scan at a different target.
//
// Every scanner needs network egress — semgrep fetches its rule set and trivy
// downloads a vulnerability database — which jobs have by default; do not give
// a scan job `network: none`.

// builtin template names — kept as exported-ish constants for callers/tests.
const (
	BuiltinSAST       = "security/sast"       // semgrep static analysis
	BuiltinDependency = "security/dependency" // trivy filesystem/dependency scan
	BuiltinContainer  = "security/container"  // trivy container image scan
	BuiltinSecrets    = "security/secrets"    // gitleaks secret detection
)

// builtinSAST runs semgrep's "auto" ruleset over the checked-out source. It
// scans in the `test` stage and never hard-blocks the pipeline by default.
const builtinSAST = `
stages: [test]
jobs:
  sast:
    stage: test
    image: returntocorp/semgrep
    script:
      - semgrep --config auto --error .
    allow_failure: true
`

// builtinDependency runs a trivy filesystem scan, which picks up lockfiles /
// manifests in the repo and reports vulnerable dependencies. It uses
// --exit-code 1 so trivy signals a non-zero status on a finding, but the job
// is allow_failure: true by default so a finding does not hard-block the
// pipeline. To make it BLOCKING, override allow_failure: false on the
// `dependency-scan` job in your main config (see docs/pipeline-dsl.md).
const builtinDependency = `
stages: [test]
jobs:
  dependency-scan:
    stage: test
    image: aquasec/trivy
    script:
      - trivy fs --exit-code 1 --no-progress .
    allow_failure: true
`

// builtinContainer runs a trivy scan of a container image. The image to scan
// is parameterized via the SCAN_IMAGE variable (default alpine:3.19, a small
// public image so the job is runnable out of the box). Override SCAN_IMAGE in
// your main config's job variables to point it at the image your pipeline
// builds/pushes. Like the dependency scan it is allow_failure: true by default.
const builtinContainer = `
stages: [test]
jobs:
  container-scan:
    stage: test
    image: aquasec/trivy
    variables:
      SCAN_IMAGE: alpine:3.19
    script:
      - trivy image --exit-code 1 --no-progress "$SCAN_IMAGE"
    allow_failure: true
`

// builtinSecrets runs gitleaks to detect committed secrets in the working
// tree. allow_failure: true by default so a hit is surfaced without blocking.
const builtinSecrets = `
stages: [test]
jobs:
  secret-detection:
    stage: test
    image: zricethezav/gitleaks
    script:
      - gitleaks detect --source . --verbose --redact
    allow_failure: true
`

// builtinTemplates maps a built-in template name to its YAML body.
var builtinTemplates = map[string]string{
	BuiltinSAST:       builtinSAST,
	BuiltinDependency: builtinDependency,
	BuiltinContainer:  builtinContainer,
	BuiltinSecrets:    builtinSecrets,
}

// BuiltinTemplate returns the YAML body of a built-in template by name, and
// whether such a built-in exists. It is the fallback consulted by include:
// resolution when the per-repo template store has no template by that name.
func BuiltinTemplate(name string) (string, bool) {
	body, ok := builtinTemplates[name]
	return body, ok
}

// ListBuiltinTemplates returns the names of all shipped built-in templates,
// sorted, for discovery (e.g. a UI or an API endpoint listing what a repo can
// include without registering anything).
func ListBuiltinTemplates() []string {
	out := make([]string, 0, len(builtinTemplates))
	for name := range builtinTemplates {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
