package compiler

import "testing"

func TestGlobRegexp(t *testing.T) {
	cases := []struct {
		pat, file string
		want      bool
	}{
		{"src/**/*.go", "src/a/b/c.go", true},
		{"src/**/*.go", "src/c.go", true},
		{"src/**/*.go", "lib/c.go", false},
		{"*.md", "README.md", true},
		{"*.md", "docs/README.md", false},
		{"**/*.md", "docs/README.md", true},
		{"docs/", "docs/a/b.txt", true},
		{"services/{cos,ecom}/**", "services/ecom/main.go", true},
		{"services/{cos,ecom}/**", "services/auth/main.go", false},
		{"Dockerfile", "Dockerfile", true},
		{"Dockerfile", "web/Dockerfile", false},
		{"go.?od", "go.mod", true},
		{"./web/**", "web/src/App.tsx", true},
	}
	for _, c := range cases {
		if got := globRegexp(c.pat).MatchString(c.file); got != c.want {
			t.Errorf("glob %q vs %q = %v, want %v", c.pat, c.file, got, c.want)
		}
	}
}

const changesYAML = `
stages: [build]
jobs:
  web:
    stage: build
    rules:
      - changes: ["web/**/*"]
    script: [make web]
  api:
    stage: build
    rules:
      - if: '$CI_COMMIT_BRANCH == "main"'
        changes: ["api/**", "go.mod"]
    script: [make api]
  always:
    stage: build
    script: [echo]
`

func TestRulesChangesEnforced(t *testing.T) {
	names := func(cs *ChangeSet, ref string) map[string]bool {
		t.Helper()
		jobs, err := CompileWithChanges(changesYAML, ref, SourcePush, nil, cs)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, j := range jobs {
			out[j.Name] = true
		}
		return out
	}
	// Unknown change set: everything runs (safe default, old behaviour).
	if got := names(nil, "main"); !got["web"] || !got["api"] || !got["always"] {
		t.Errorf("unknown change set = %v, want all", got)
	}
	// Only web changed: api is skipped.
	got := names(&ChangeSet{Files: []string{"web/src/App.tsx"}}, "main")
	if !got["web"] || got["api"] || !got["always"] {
		t.Errorf("web change = %v", got)
	}
	// go.mod changed on main: api runs, web doesn't.
	got = names(&ChangeSet{Files: []string{"go.mod"}}, "main")
	if got["web"] || !got["api"] {
		t.Errorf("go.mod change = %v", got)
	}
	// if: and changes: combine with AND.
	got = names(&ChangeSet{Files: []string{"api/x.go"}}, "feature")
	if got["api"] {
		t.Errorf("api on a feature branch should not run (if: false): %v", got)
	}
	// Nothing relevant changed.
	got = names(&ChangeSet{Files: []string{"README.md"}}, "main")
	if got["web"] || got["api"] || !got["always"] {
		t.Errorf("README-only change = %v", got)
	}
}
