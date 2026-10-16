package api

import (
	"strings"
	"testing"
)

func TestLintEndpoint(t *testing.T) {
	h := newEnforcedHarness(t)
	dev := h.login("dev@acme.io")
	admin := h.login("admin@acme.io")

	// A valid config comes back as the jobs that would run on the ref.
	rec := h.do(dev, "POST", "/api/v1/lint", map[string]any{"config": policyYAML, "ref": "feature/x"})
	h.expect(rec, 200, "lint valid")
	got := decodeInto[lintResponse](t, rec)
	if !got.Valid || len(got.Jobs) != 4 || strings.Join(got.Stages, ",") != "dev,staging,prod,sandbox" {
		t.Fatalf("lint = %+v", got)
	}

	// Broken YAML / bad references are reported, not a 4xx.
	rec = h.do(dev, "POST", "/api/v1/lint", map[string]any{"config": "stages: [a]\njobs:\n  x:\n    stage: nope\n    script: [y]\n"})
	h.expect(rec, 200, "lint invalid")
	if bad := decodeInto[lintResponse](t, rec); bad.Valid || bad.Error == "" {
		t.Errorf("invalid config = %+v, want valid=false with an error", bad)
	}
	h.expect(h.do(dev, "POST", "/api/v1/lint", map[string]any{"config": ""}), 400, "empty config")

	// With an environment policy, lint previews what would be refused.
	h.expect(h.do(admin, "PUT", "/api/v1/environment-registry", testPolicy), 200, "save registry")
	rec = h.do(dev, "POST", "/api/v1/lint", map[string]any{"config": policyYAML, "ref": "feature/x"})
	byName := map[string]lintJob{}
	for _, j := range decodeInto[lintResponse](t, rec).Jobs {
		byName[j.Name] = j
	}
	if byName["deploy_dev"].Refused != "" {
		t.Errorf("deploy_dev should be allowed: %q", byName["deploy_dev"].Refused)
	}
	if p := byName["deploy_prod"]; p.Environment != "production" || !strings.Contains(p.Refused, "may not deploy") {
		t.Errorf("deploy_prod = %+v, want canonical production and a refusal", p)
	}
	if !strings.Contains(byName["deploy_sandbox"].Refused, "not registered") {
		t.Errorf("deploy_sandbox refusal = %q", byName["deploy_sandbox"].Refused)
	}
	h.expect(h.do(testCaller{}, "POST", "/api/v1/lint", map[string]any{"config": policyYAML}), 401, "anonymous lint")
}
