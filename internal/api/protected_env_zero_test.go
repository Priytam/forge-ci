package api

import (
	"context"
	"strings"
	"testing"

	"github.com/priytamjeepandey/forge-ci/internal/compiler"
	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

// TestProtectedEnvZeroApprovals: required_approvals 0 is kept (not turned
// into 1), a repo's 0-approval rule beats the global 1-approval default so
// its deploy jobs run without a vote, and bad values are 400s.
func TestProtectedEnvZeroApprovals(t *testing.T) {
	h := newEnforcedHarness(t)
	ctx := context.Background()
	admin := h.login("admin@acme.io")

	rec := h.do(admin, "POST", "/api/v1/protected-environments", map[string]any{"repo": "acme/x", "name": "production", "required_approvals": 0})
	h.expect(rec, 200, "0 approvals")
	if got := decodeInto[proto.ProtectedEnvironment](t, rec); got.RequiredApprovals != 0 {
		t.Fatalf("required_approvals = %d, want 0", got.RequiredApprovals)
	}
	rec = h.do(admin, "POST", "/api/v1/protected-environments", map[string]any{"repo": "acme/y", "name": "production"})
	h.expect(rec, 200, "default")
	if got := decodeInto[proto.ProtectedEnvironment](t, rec); got.RequiredApprovals != 1 || got.ApprovalTimeoutHours != 24 {
		t.Fatalf("omitted fields = %+v, want 1 approval, 24h", got)
	}
	for _, bad := range []map[string]any{
		{"name": "production", "required_approvals": -1},
		{"name": "production", "required_approvals": 21},
		{"name": "production", "approval_timeout_hours": -2},
		{"name": "production", "required_approvals": 0, "required_group": "release-approvers"},
	} {
		h.expect(h.do(admin, "POST", "/api/v1/protected-environments", bad), 400, "invalid rule")
	}
	rec = h.do(admin, "POST", "/api/v1/protected-environments", map[string]any{"name": "production", "required_approvals": -1})
	if !strings.Contains(rec.Body.String(), "between 0 and 20") {
		t.Errorf("error = %s", rec.Body.String())
	}

	yml := "stages: [d]\njobs:\n  ship:\n    stage: d\n    environment: production\n    script: [x]\n"
	status := func(repo string) string {
		jobs, _ := compiler.Compile(yml, "main", compiler.SourceAPI, nil)
		p, err := h.st.CreatePipeline(ctx, proto.CreatePipelineRequest{Repo: repo, Ref: "main", SHA: "s", Config: yml}, jobs, nil, false, false)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = h.st.PromoteReadyJobs(ctx)
		_, js, _ := h.st.GetPipeline(ctx, p.ID)
		return js[0].Status
	}
	if s := status("acme/x"); s != "pending" {
		t.Errorf("0-approval repo: job %s, want pending", s)
	}
	if s := status("acme/y"); s != "blocked" {
		t.Errorf("1-approval repo: job %s, want blocked", s)
	}
	if s := status("acme/z"); s != "blocked" {
		t.Errorf("global default (production, 1 approval): job %s, want blocked", s)
	}
}
