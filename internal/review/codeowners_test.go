package review

import (
	"strings"
	"testing"

	"github.com/priytamjeepandey/forge-ci/internal/store"
)

func TestCodeOwnersPatterns(t *testing.T) {
	rules, bad := ParseCodeOwners(`# owners
*                 @acme/everyone
*.go              @go-team
/build/           @release      # only the top-level build dir
docs/*            docs@acme.io
apps/**/api       @api
**/secrets.yml    @security
/vendor/
[Section]
README.md         @Writers
`)
	if len(bad) != 0 {
		t.Fatalf("bad lines: %v", bad)
	}
	cases := map[string]string{
		"main.go":                  "@go-team",
		"pkg/x/y.go":               "@go-team",
		"build/out.txt":            "@release",
		"src/build/out.txt":        "@acme/everyone", // anchored: only the root build/
		"docs/a.md":                "docs@acme.io",
		"docs/deep/a.md":           "@acme/everyone", // docs/* does not reach below
		"apps/web/v2/api/h.ts":     "@api",
		"apps/api/h.ts":            "@api",
		"conf/prod/secrets.yml":    "@security",
		"vendor/lib/x.go":          "", // owned by nobody (last match, no owners)
		"README.md":                "@writers",
		"notes/README.md":          "@writers",
		"cmd/server/main_test.txt": "@acme/everyone",
	}
	for path, want := range cases {
		owners, ok := OwnersOf(rules, path)
		if got := strings.Join(owners, " "); !ok || got != want {
			t.Errorf("%s: owners %q (matched %v), want %q", path, got, ok, want)
		}
	}
}

func TestCodeOwnersRuleStates(t *testing.T) {
	res := ownerResolver{identities: map[string]string{"ana-gh": "ana@acme.io"},
		groups: map[string]map[string]bool{"backend": {"bo@acme.io": true}}}
	ready := func(content string, paths ...string) *store.CodeOwnersInput {
		return &store.CodeOwnersInput{State: "ready", PathsKnown: true, OwnersRead: true, File: "CODEOWNERS",
			Content: content, Paths: paths}
	}
	cases := []struct {
		name      string
		in        *store.CodeOwnersInput
		approvers []approver
		state     string
		detail    string
	}{
		{"diff pending", &store.CodeOwnersInput{State: "pending"}, nil, "pending", "Working out"},
		{"old runner", &store.CodeOwnersInput{State: "ready", PathsKnown: true}, nil, "pending", "upgraded"},
		{"no file", &store.CodeOwnersInput{State: "ready", PathsKnown: true, OwnersRead: true}, nil, "pass", "No CODEOWNERS file on main"},
		{"unowned files", ready("*.go @backend\n", "a.md"), nil, "pass", "No touched file has a code owner"},
		{"host login approves", ready("*.go @ana-gh\n", "a.go"), []approver{{login: "ana-gh"}}, "pass", "approved all 1"},
		{"linked email approves", ready("*.go @ana-gh\n", "a.go"), []approver{{email: "ana@acme.io"}}, "pass", ""},
		{"team via group", ready("*.go @acme/backend\n", "a.go", "b.go"), []approver{{email: "bo@acme.io"}}, "pass", "all 2"},
		{"wrong person", ready("*.go @acme/backend\n", "a.go", "b.go"), []approver{{email: "ana@acme.io"}}, "pending",
			"@acme/backend for a.go and 1 more"},
		{"unknown owner", ready("*.go @ghost\n", "a.go"), nil, "pending", "no Forge user for @ghost"},
		{"bare group name", ready("*.go @backend\n", "a.go"), []approver{{email: "bo@acme.io"}}, "pass", ""},
	}
	for _, c := range cases {
		state, detail := codeOwnersRule(c.in, "main", res, c.approvers)
		if state != c.state || !strings.Contains(detail, c.detail) {
			t.Errorf("%s: %s %q, want %s ~%q", c.name, state, detail, c.state, c.detail)
		}
	}
}
