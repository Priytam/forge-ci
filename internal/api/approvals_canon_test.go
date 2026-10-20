package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/priytamjeepandey/forge-ci/internal/compiler"
	"github.com/priytamjeepandey/forge-ci/internal/proto"
	"github.com/priytamjeepandey/forge-ci/internal/store"
)

// TestApprovalRulesFoldIntoCanonicalName: a rule saved as "production"
// before the registry existed, next to the real "Production" rule, is folded
// into it with the stricter settings, and a job under either spelling meets
// the strict rule.
func TestApprovalRulesFoldIntoCanonicalName(t *testing.T) {
	h := newEnforcedHarness(t)
	ctx := context.Background()
	// The pre-registry world: two rows, one weak, one strict, differing only in case.
	_, _ = h.db.Exec(ctx, `DELETE FROM protected_environments`)
	_, _ = h.db.Exec(ctx, `INSERT INTO protected_environments (repo, name, required_approvals, approval_timeout_hours, allow_self_approval)
	                        VALUES ('', 'production', 1, 48, TRUE), ('', 'Production', 2, 24, FALSE)`)
	_, _ = h.db.Exec(ctx, `UPDATE protected_environments SET required_group='release-approvers', required_group_approvals=1 WHERE name='Production'`)
	if err := h.st.SaveEnvPolicy(ctx, store.EnvPolicy{Environments: []store.RegisteredEnv{{Name: "Production", Tier: "prod", Aliases: []string{"prod"}}}}); err != nil {
		t.Fatal(err)
	}
	rules, _ := h.st.ListProtectedEnvs(ctx, "acme/x")
	if len(rules) != 1 || rules[0].Name != "Production" || rules[0].RequiredApprovals != 2 || rules[0].AllowSelfApproval || rules[0].ApprovalTimeoutHours != 24 || rules[0].RequiredGroup != "release-approvers" {
		t.Fatalf("after fold = %+v", rules)
	}
	// A job stored under the old spelling still meets the strict rule.
	_, _ = h.st.AddMember(ctx, "acme/x", "owner@acme.io", "owner")
	yml := "stages: [d]\njobs:\n  ship:\n    stage: d\n    environment: production\n    script: [x]\n"
	jobs, _ := compiler.Compile(yml, "main", compiler.SourceAPI, nil)
	p, err := h.st.CreatePipeline(ctx, proto.CreatePipelineRequest{Repo: "acme/x", Ref: "main", SHA: "s", Config: yml}, jobs, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = h.db.Exec(ctx, `UPDATE jobs SET environment='production' WHERE pipeline_id=$1`, p.ID) // force the raw spelling
	_, _ = h.st.PromoteReadyJobs(ctx)
	_, js, _ := h.st.GetPipeline(ctx, p.ID)
	if js[0].Status != "blocked" || js[0].RequiredApprovals != 2 || js[0].RequiredGroup != "release-approvers" {
		t.Errorf("job under raw spelling = %s needs %d group %q, want blocked / 2 / release-approvers", js[0].Status, js[0].RequiredApprovals, js[0].RequiredGroup)
	}
	// A rule that matches nothing registered is marked inactive.
	_, _ = h.db.Exec(ctx, `INSERT INTO protected_environments (repo, name, required_approvals) VALUES ('', 'legacy-prod', 1)`)
	rules, _ = h.st.ListProtectedEnvs(ctx, "acme/x")
	for _, r := range rules {
		if (r.Name == "legacy-prod") != r.Inactive {
			t.Errorf("rule %s inactive=%v", r.Name, r.Inactive)
		}
	}
}

func TestJobDetailShowsGroupProgress(t *testing.T) {
	h := newEnforcedHarness(t)
	ctx := context.Background()
	admin := h.login("admin@acme.io")
	h.expect(h.do(admin, "PUT", "/api/v1/approver-groups/release-approvers", map[string]any{"members": []string{"ravi@acme.io", "ashwin@acme.io"}}), 200, "")
	h.expect(h.do(admin, "POST", "/api/v1/protected-environments", map[string]any{
		"name": "production", "required_approvals": 2, "required_group": "release-approvers", "required_group_approvals": 1, "approver_roles": []string{"owner", "admin"},
	}), 200, "")
	_, _ = h.st.AddMember(ctx, "acme/x", "owner@acme.io", "owner")
	_, _ = h.st.AddMember(ctx, "acme/x", "ravi@acme.io", "owner")
	yml := "stages: [d]\njobs:\n  ship:\n    stage: d\n    environment: production\n    script: [x]\n"
	jobs, _ := compiler.Compile(yml, "main", compiler.SourceAPI, nil)
	p, _ := h.st.CreatePipeline(ctx, proto.CreatePipelineRequest{Repo: "acme/x", Ref: "main", SHA: "s", Config: yml, TriggeredBy: "dev@acme.io"}, jobs, nil, false, false)
	_, _ = h.st.PromoteReadyJobs(ctx)
	_, js, _ := h.st.GetPipeline(ctx, p.ID)
	job := js[0]
	if _, err := h.st.Approve(ctx, job.ID, proto.ApprovalRequest{Approver: "owner@acme.io", Verdict: "approved"}); err != nil {
		t.Fatal(err)
	}
	// Two paths, one answer: the job detail endpoint and the pipeline endpoint.
	for _, path := range []string{"/api/v1/jobs/" + itoa64(job.ID), "/api/v1/pipelines/" + itoa64(p.ID)} {
		body := h.do(admin, "GET", path, nil).Body.String()
		if !strings.Contains(body, `"required_group":"release-approvers"`) || !strings.Contains(body, `"required_group_approvals":1`) ||
			!strings.Contains(body, `"group_members":["ashwin@acme.io","ravi@acme.io"]`) || strings.Contains(body, `"group_approvals":1`) {
			t.Errorf("%s: %s", path, body)
		}
	}
	// One more vote, from the group: progress shows and the job releases.
	if _, err := h.st.Approve(ctx, job.ID, proto.ApprovalRequest{Approver: "ravi@acme.io", Verdict: "approved"}); err != nil {
		t.Fatal(err)
	}
	after, _ := h.st.GetJob(ctx, job.ID)
	if after.GroupApprovals != 1 || after.Status == "blocked" {
		t.Errorf("after group vote = %+v", after)
	}
}

func TestSessionRefreshAndNextRedirect(t *testing.T) {
	h := newEnforcedHarness(t)
	dev := h.login("dev@acme.io")
	rec := h.do(dev, "POST", "/api/v1/auth/refresh", nil)
	h.expect(rec, 200, "")
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["expires_at"] == nil {
		t.Errorf("refresh = %s", rec.Body.String())
	}
	if rec := h.do(testCaller{}, "POST", "/api/v1/auth/refresh", nil); rec.Code != 401 {
		t.Errorf("refresh without session = %d", rec.Code)
	}
	// ?next= is remembered on login start and used by the callback; off-site values are ignored.
	for next, want := range map[string]string{"/pipelines/42": "/pipelines/42", "https://evil.example": "", "//evil": ""} {
		rec := httptest.NewRecorder()
		h.srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/auth/login/google?next="+next, nil))
		var got string
		for _, c := range rec.Result().Cookies() {
			if c.Name == "forge_next" {
				got = c.Value
			}
		}
		if got != want {
			t.Errorf("next=%q remembered as %q, want %q", next, got, want)
		}
	}
}

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }
