package compiler

import "testing"

// A minimal main config that includes a built-in and adds one normal build
// job. The build job lives in its own stage so we can confirm the built-in's
// scan job is merged in alongside it.
func includeMain(template string) string {
	return `
include:
  - template: ` + template + `
stages: [build]
jobs:
  build:
    stage: build
    image: golang:1.23
    script: [go build ./...]
`
}

// TestBuiltinTemplatesResolveWithNilResolver proves every built-in security
// template compiles into the expected scan job WITHOUT any repo template store
// (nil TemplateFunc) — i.e. built-ins are available to repos with zero
// registered templates.
func TestBuiltinTemplatesResolveWithNilResolver(t *testing.T) {
	cases := []struct {
		template string
		job      string
	}{
		{BuiltinSAST, "sast"},
		{BuiltinDependency, "dependency-scan"},
		{BuiltinContainer, "container-scan"},
		{BuiltinSecrets, "secret-detection"},
	}
	for _, tc := range cases {
		t.Run(tc.template, func(t *testing.T) {
			jobs, err := Compile(includeMain(tc.template), "main", "api", nil)
			if err != nil {
				t.Fatalf("compile with nil resolver: %v", err)
			}
			m := jobNames(jobs)
			scan, ok := m[tc.job]
			if !ok {
				t.Fatalf("expected scan job %q in DAG, got jobs %v", tc.job, keys(m))
			}
			if !scan.AllowFailure {
				t.Errorf("scan job %q: expected allow_failure=true by default", tc.job)
			}
			if scan.Stage != "test" {
				t.Errorf("scan job %q: expected stage=test, got %q", tc.job, scan.Stage)
			}
			if scan.Image == "" {
				t.Errorf("scan job %q: expected a scanner image to be set", tc.job)
			}
			if _, ok := m["build"]; !ok {
				t.Errorf("expected the main config's build job to survive the merge")
			}
		})
	}
}

// TestBuiltinTemplatesResolveWithRepoResolverPresent proves the same built-ins
// resolve when a repo resolver IS present but does not have the named template
// (falls through to the built-in).
func TestBuiltinTemplatesResolveWithRepoResolverPresent(t *testing.T) {
	// A resolver that knows about "other/thing" but none of the built-ins.
	resolver := func(name string) (string, bool, error) {
		if name == "other/thing" {
			return "stages: [lint]\njobs:\n  lint:\n    stage: lint\n    script: [echo lint]\n", true, nil
		}
		return "", false, nil
	}
	jobs, err := Compile(includeMain(BuiltinSAST), "main", "api", resolver)
	if err != nil {
		t.Fatalf("compile with repo resolver present: %v", err)
	}
	m := jobNames(jobs)
	if scan, ok := m["sast"]; !ok {
		t.Fatalf("expected built-in sast job to resolve through a non-matching resolver, got %v", keys(m))
	} else if !scan.AllowFailure {
		t.Errorf("sast: expected allow_failure=true")
	}
}

// TestRepoTemplateOverridesBuiltin proves a per-repo template of the same name
// WINS over the built-in (precedence: repo store first).
func TestRepoTemplateOverridesBuiltin(t *testing.T) {
	// The repo registers its OWN "security/sast" that emits a different job in
	// a different stage — this must shadow the built-in entirely.
	override := "stages: [audit]\njobs:\n  custom-sast:\n    stage: audit\n    image: my/scanner\n    script: [my-scan]\n"
	resolver := func(name string) (string, bool, error) {
		if name == BuiltinSAST {
			return override, true, nil
		}
		return "", false, nil
	}
	jobs, err := Compile(includeMain(BuiltinSAST), "main", "api", resolver)
	if err != nil {
		t.Fatalf("compile with overriding repo template: %v", err)
	}
	m := jobNames(jobs)
	if _, ok := m["custom-sast"]; !ok {
		t.Fatalf("expected repo override job custom-sast, got %v", keys(m))
	}
	if _, ok := m["sast"]; ok {
		t.Errorf("built-in sast job must NOT appear when the repo shadows security/sast")
	}
}

// TestContainerScanVariableOverride proves the SCAN_IMAGE variable default is
// present and can be overridden by the main config (deep-merged variables).
func TestContainerScanVariableOverride(t *testing.T) {
	// Default: SCAN_IMAGE is set by the built-in.
	jobs, err := Compile(includeMain(BuiltinContainer), "main", "api", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := jobNames(jobs)["container-scan"].Env["SCAN_IMAGE"]; got != "alpine:3.19" {
		t.Errorf("container-scan default SCAN_IMAGE: want alpine:3.19, got %q", got)
	}

	// Override via the main config's job variables (main wins on merge).
	yml := `
include:
  - template: security/container
stages: [test]
jobs:
  container-scan:
    variables:
      SCAN_IMAGE: myorg/app:latest
`
	jobs, err = Compile(yml, "main", "api", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := jobNames(jobs)["container-scan"].Env["SCAN_IMAGE"]; got != "myorg/app:latest" {
		t.Errorf("container-scan overridden SCAN_IMAGE: want myorg/app:latest, got %q", got)
	}
}

// TestOverrideAllowFailureBlocking proves a repo can flip a scan to blocking by
// setting allow_failure: false on the scan job in its main config.
func TestOverrideAllowFailureBlocking(t *testing.T) {
	yml := `
include:
  - template: security/dependency
stages: [test]
jobs:
  dependency-scan:
    allow_failure: false
`
	jobs, err := Compile(yml, "main", "api", nil)
	if err != nil {
		t.Fatal(err)
	}
	if scan := jobNames(jobs)["dependency-scan"]; scan.AllowFailure {
		t.Errorf("dependency-scan: allow_failure should be overridable to false, got true")
	}
}

func TestListBuiltinTemplates(t *testing.T) {
	got := ListBuiltinTemplates()
	want := []string{"security/container", "security/dependency", "security/sast", "security/secrets"}
	if len(got) != len(want) {
		t.Fatalf("ListBuiltinTemplates: want %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ListBuiltinTemplates[%d]: want %q, got %q", i, want[i], got[i])
		}
	}
}

func keys(m map[string]CompiledJob) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
