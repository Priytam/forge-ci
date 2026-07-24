package compiler

import (
	"sort"
	"strings"
	"testing"
)

// mapResolver builds a TemplateFunc from an in-memory map, for include: tests.
func mapResolver(m map[string]string) TemplateFunc {
	return func(name string) (string, bool, error) {
		y, ok := m[name]
		return y, ok, nil
	}
}

// ---- rules: ----

func TestRulesFirstMatchAndRefAware(t *testing.T) {
	yml := `
stages: [test, deploy]
jobs:
  unit:
    stage: test
    script: [make test]
  deploy:
    stage: deploy
    script: [./deploy.sh]
    rules:
      - if: '$CI_COMMIT_BRANCH == "main"'
        when: on_success
      - when: never
`
	// On main: deploy included.
	main, err := Compile(yml, "main", SourceWebhook, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := jobNames(main)["deploy"]; !ok {
		t.Error("main: deploy should be included by first rule")
	}
	// On a feature branch: first rule fails, second (when: never) excludes it.
	feat, err := Compile(yml, "feature/x", SourceWebhook, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := jobNames(feat)["deploy"]; ok {
		t.Error("feature: deploy should be excluded (when: never)")
	}
}

func TestRulesNoMatchExcludes(t *testing.T) {
	yml := `
stages: [build]
jobs:
  always-there:
    stage: build
    script: [echo hi]
  maybe:
    stage: build
    script: [echo maybe]
    rules:
      - if: '$CI_COMMIT_BRANCH == "release"'
`
	jobs, err := Compile(yml, "main", SourceAPI, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := jobNames(jobs)["maybe"]; ok {
		t.Error("maybe should be excluded when no rule matches")
	}
	if _, ok := jobNames(jobs)["always-there"]; !ok {
		t.Error("always-there should be present")
	}
}

func TestRulesWhenManual(t *testing.T) {
	yml := `
stages: [deploy]
jobs:
  deploy:
    stage: deploy
    script: [./deploy.sh]
    rules:
      - if: '$CI_COMMIT_BRANCH == "main"'
        when: manual
`
	jobs, err := Compile(yml, "main", SourceAPI, nil)
	if err != nil {
		t.Fatal(err)
	}
	d, ok := jobNames(jobs)["deploy"]
	if !ok {
		t.Fatal("deploy should be included")
	}
	if !d.Manual {
		t.Error("deploy should be marked manual")
	}
}

func TestRulesPipelineSource(t *testing.T) {
	yml := `
stages: [build]
jobs:
  api-only:
    stage: build
    script: [echo hi]
    rules:
      - if: '$CI_PIPELINE_SOURCE == "api"'
`
	if jobs, _ := Compile(yml, "main", SourceAPI, nil); len(jobs) != 1 {
		t.Errorf("api source: expected 1 job, got %d", len(jobs))
	}
	if _, err := Compile(yml, "main", SourceWebhook, nil); err == nil {
		t.Error("webhook source: expected error (no jobs match)")
	}
}

func TestRulesSupersedeOnlyExcept(t *testing.T) {
	// only says "release-*" but rules say "main"; rules win.
	yml := `
stages: [build]
jobs:
  j:
    stage: build
    script: [echo hi]
    only: [release-*]
    rules:
      - if: '$CI_COMMIT_BRANCH == "main"'
`
	if _, err := Compile(yml, "main", SourceAPI, nil); err != nil {
		t.Errorf("rules should include on main despite only:release-* : %v", err)
	}
	if _, err := Compile(yml, "release-1", SourceAPI, nil); err == nil {
		t.Error("rules should exclude on release-1 despite only:release-*")
	}
}

func TestRulesAllowFailure(t *testing.T) {
	yml := `
stages: [test]
jobs:
  flaky:
    stage: test
    script: [make test]
    rules:
      - if: '$CI_COMMIT_BRANCH == "main"'
        allow_failure: true
`
	jobs, err := Compile(yml, "main", SourceAPI, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !jobNames(jobs)["flaky"].AllowFailure {
		t.Error("flaky should have AllowFailure=true from the matching rule")
	}
}

func TestRulesInvalidExprAndWhen(t *testing.T) {
	badExpr := `
stages: [b]
jobs:
  j:
    stage: b
    script: [x]
    rules:
      - if: '$X =='
`
	if _, err := Compile(badExpr, "main", SourceAPI, nil); err == nil {
		t.Error("expected error for malformed rules if:")
	}
	badWhen := `
stages: [b]
jobs:
  j:
    stage: b
    script: [x]
    rules:
      - when: sometimes
`
	if _, err := Compile(badWhen, "main", SourceAPI, nil); err == nil {
		t.Error("expected error for invalid when:")
	}
}

// ---- include: ----

func TestIncludeMergeAndOverride(t *testing.T) {
	tmpl := mapResolver(map[string]string{
		"base": `
stages: [build, test]
default:
  timeout: 30m
jobs:
  build-app:
    stage: build
    image: golang:1.22
    script: [make build]
  unit:
    stage: test
    script: [make test]
`,
	})
	main := `
include:
  - template: base
stages: [build, test, deploy]
jobs:
  build-app:
    image: golang:1.23     # override just the image; script inherited
  deploy:
    stage: deploy
    script: [./deploy.sh]
`
	jobs, err := Compile(main, "main", SourceAPI, tmpl)
	if err != nil {
		t.Fatal(err)
	}
	m := jobNames(jobs)
	if len(m) != 3 {
		t.Fatalf("expected 3 jobs (build-app, unit, deploy), got %d: %v", len(m), keysOf(m))
	}
	// build-app: image overridden by main, script inherited from template.
	if m["build-app"].Image != "golang:1.23" {
		t.Errorf("build-app image: want golang:1.23 (main override), got %q", m["build-app"].Image)
	}
	if m["build-app"].Script != "make build" {
		t.Errorf("build-app script: want inherited 'make build', got %q", m["build-app"].Script)
	}
	// unit came purely from the template.
	if _, ok := m["unit"]; !ok {
		t.Error("unit should be merged in from the template")
	}
	// deploy stage from main; stages union preserved order.
	if m["deploy"].Stage != "deploy" {
		t.Errorf("deploy stage: %q", m["deploy"].Stage)
	}
}

func TestIncludeMissingTemplateAndNoResolver(t *testing.T) {
	main := `
include:
  - template: nope
stages: [b]
jobs:
  j: {stage: b, script: [x]}
`
	if _, err := Compile(main, "main", SourceAPI, mapResolver(map[string]string{})); err == nil {
		t.Error("expected error for unregistered template")
	}
	if _, err := Compile(main, "main", SourceAPI, nil); err == nil {
		t.Error("expected error when include used with no resolver")
	}
}

// ---- extends: ----

func TestExtendsSingleHiddenBase(t *testing.T) {
	yml := `
stages: [test]
jobs:
  .base:
    image: golang:1.22
    variables: {A: "1"}
    script: [make base]
  unit:
    stage: test
    extends: .base
    script: [make test]
`
	jobs, err := Compile(yml, "main", SourceAPI, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := jobNames(jobs)
	if _, ok := m[".base"]; ok {
		t.Error(".base is a hidden template and must not be emitted")
	}
	u := m["unit"]
	if u.Image != "golang:1.22" {
		t.Errorf("unit image: want inherited golang:1.22, got %q", u.Image)
	}
	if u.Script != "make test" { // child overrides parent script
		t.Errorf("unit script: want child 'make test', got %q", u.Script)
	}
	if u.Env["A"] != "1" {
		t.Errorf("unit should inherit variable A=1, got %v", u.Env)
	}
}

func TestExtendsMultipleLeftToRight(t *testing.T) {
	yml := `
stages: [test]
jobs:
  .a:
    image: img-a
    variables: {X: "a", COMMON: "a"}
    script: [a]
  .b:
    image: img-b
    variables: {Y: "b", COMMON: "b"}
  job:
    stage: test
    extends: [.a, .b]
`
	jobs, err := Compile(yml, "main", SourceAPI, nil)
	if err != nil {
		t.Fatal(err)
	}
	j := jobNames(jobs)["job"]
	// later parent (.b) overrides earlier (.a) on conflicts.
	if j.Image != "img-b" {
		t.Errorf("image: want img-b (later parent wins), got %q", j.Image)
	}
	if j.Env["COMMON"] != "b" {
		t.Errorf("COMMON: want b (later parent wins), got %q", j.Env["COMMON"])
	}
	if j.Env["X"] != "a" || j.Env["Y"] != "b" {
		t.Errorf("variables union wrong: %v", j.Env)
	}
	if j.Script != "a" { // script only defined in .a
		t.Errorf("script: want inherited 'a', got %q", j.Script)
	}
}

func TestExtendsChain(t *testing.T) {
	yml := `
stages: [test]
jobs:
  .grand:
    image: base-img
    variables: {LEVEL: "grand", G: "1"}
    script: [grand]
  .parent:
    extends: .grand
    variables: {LEVEL: "parent", P: "1"}
  child:
    stage: test
    extends: .parent
    variables: {LEVEL: "child"}
`
	jobs, err := Compile(yml, "main", SourceAPI, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := jobNames(jobs)["child"]
	if c.Image != "base-img" {
		t.Errorf("image should chain from .grand, got %q", c.Image)
	}
	if c.Env["LEVEL"] != "child" {
		t.Errorf("LEVEL should be overridden to child, got %q", c.Env["LEVEL"])
	}
	if c.Env["G"] != "1" || c.Env["P"] != "1" {
		t.Errorf("chain variables missing: %v", c.Env)
	}
	if c.Script != "grand" {
		t.Errorf("script should chain from .grand, got %q", c.Script)
	}
}

func TestExtendsCycleAndUnknown(t *testing.T) {
	cycle := `
stages: [t]
jobs:
  .a: {extends: .b, script: [x]}
  .b: {extends: .a, script: [y]}
  j: {stage: t, extends: .a}
`
	if _, err := Compile(cycle, "main", SourceAPI, nil); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Errorf("expected extends cycle error, got %v", err)
	}
	unknown := `
stages: [t]
jobs:
  j: {stage: t, extends: .nope, script: [x]}
`
	if _, err := Compile(unknown, "main", SourceAPI, nil); err == nil {
		t.Error("expected error for extends of unknown job")
	}
}

// ---- parallel / matrix ----

func TestParallelN(t *testing.T) {
	yml := `
stages: [test]
jobs:
  spec:
    stage: test
    parallel: 3
    script: [run-tests]
`
	jobs, err := Compile(yml, "main", SourceAPI, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 3 {
		t.Fatalf("parallel:3 should yield 3 jobs, got %d", len(jobs))
	}
	names := jobNamesList(jobs)
	want := []string{"spec 1/3", "spec 2/3", "spec 3/3"}
	sort.Strings(names)
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("instance name[%d] = %q, want %q", i, names[i], want[i])
		}
	}
	m := jobNames(jobs)
	if m["spec 2/3"].Env["CI_NODE_INDEX"] != "2" || m["spec 2/3"].Env["CI_NODE_TOTAL"] != "3" {
		t.Errorf("spec 2/3 node vars wrong: %v", m["spec 2/3"].Env)
	}
}

func TestMatrixCartesian(t *testing.T) {
	yml := `
stages: [build]
jobs:
  build:
    stage: build
    script: [make]
    parallel:
      matrix:
        - OS: [linux, windows]
          ARCH: [amd64, arm64]
`
	jobs, err := Compile(yml, "main", SourceAPI, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 4 {
		t.Fatalf("2x2 matrix should yield 4 jobs, got %d: %v", len(jobs), jobNamesList(jobs))
	}
	m := jobNames(jobs)
	// name is "build: [ARCH, OS]" (values in key-sorted order: ARCH, OS).
	inst, ok := m["build: [amd64, linux]"]
	if !ok {
		t.Fatalf("expected instance 'build: [amd64, linux]', got %v", jobNamesList(jobs))
	}
	if inst.Env["OS"] != "linux" || inst.Env["ARCH"] != "amd64" {
		t.Errorf("matrix vars not injected: %v", inst.Env)
	}
	// every instance is unique
	seen := map[string]bool{}
	for _, n := range jobNamesList(jobs) {
		if seen[n] {
			t.Errorf("duplicate instance name %q", n)
		}
		seen[n] = true
	}
}

func TestMatrixNeedsFanIn(t *testing.T) {
	yml := `
stages: [build, deploy]
jobs:
  build:
    stage: build
    script: [make]
    parallel:
      matrix:
        - OS: [linux, windows]
  deploy:
    stage: deploy
    needs: [build]
    script: [./deploy.sh]
`
	jobs, err := Compile(yml, "main", SourceAPI, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := jobNames(jobs)
	d, ok := m["deploy"]
	if !ok {
		t.Fatal("deploy missing")
	}
	if len(d.Needs) != 2 {
		t.Fatalf("deploy should fan-in to both build instances, got needs=%v", d.Needs)
	}
	sort.Strings(d.Needs)
	want := []string{"build: [linux]", "build: [windows]"}
	for i := range want {
		if d.Needs[i] != want[i] {
			t.Errorf("deploy needs[%d]=%q, want %q", i, d.Needs[i], want[i])
		}
	}
}

func TestMatrixImplicitNeedsFanIn(t *testing.T) {
	// deploy has no explicit needs -> implicit needs = all instances of the
	// previous stage (both matrix instances of build).
	yml := `
stages: [build, deploy]
jobs:
  build:
    stage: build
    script: [make]
    parallel: 2
  deploy:
    stage: deploy
    script: [./deploy.sh]
`
	jobs, err := Compile(yml, "main", SourceAPI, nil)
	if err != nil {
		t.Fatal(err)
	}
	d := jobNames(jobs)["deploy"]
	if len(d.Needs) != 2 {
		t.Errorf("deploy implicit needs should fan-in to 2 build instances, got %v", d.Needs)
	}
}

func TestMatrixBadValues(t *testing.T) {
	bad := `
stages: [b]
jobs:
  j:
    stage: b
    script: [x]
    parallel:
      matrix:
        - OS: linux
          BROKEN: {a: 1}
`
	if _, err := Compile(bad, "main", SourceAPI, nil); err == nil {
		t.Error("expected error for non-scalar/non-list matrix value")
	}
}

// helpers

func jobNamesList(jobs []CompiledJob) []string {
	out := make([]string, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, j.Name)
	}
	return out
}

func keysOf(m map[string]CompiledJob) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
