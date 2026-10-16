package api

import (
	"context"
	"testing"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

// TestApprovalNeedsGroupMember: "2 approvals, at least 1 from
// release-managers". Two owners approving is not enough; a release manager
// (who has no repo role) may vote and their vote releases the job.
func TestApprovalNeedsGroupMember(t *testing.T) {
	h := newEnforcedHarness(t)
	ctx := context.Background()
	admin := h.login("admin@acme.io")
	const repo = "acme/ecom"
	for _, u := range []string{"o1@acme.io", "o2@acme.io"} {
		if _, err := h.st.AddMember(ctx, repo, u, "owner"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.st.AddMember(ctx, repo, "dev@acme.io", "developer"); err != nil {
		t.Fatal(err)
	}

	h.expect(h.do(h.login("dev@acme.io"), "PUT", "/api/v1/approver-groups/release-managers",
		map[string]any{"members": []string{"Harish@acme.io"}}), 403, "dev edits groups")
	h.expect(h.do(admin, "PUT", "/api/v1/approver-groups/release-managers", map[string]any{
		"description": "Harish and Ashwin", "members": []string{"Harish@acme.io", "ashwin@acme.io", "harish@acme.io"},
	}), 200, "admin creates group")
	h.expect(h.do(admin, "POST", "/api/v1/protected-environments", map[string]any{
		"repo": repo, "name": "production", "required_approvals": 2, "approver_roles": []string{"owner"},
		"allow_self_approval": true, "required_group": "nope", "required_group_approvals": 1,
	}), 400, "rule naming a missing group")
	h.expect(h.do(admin, "POST", "/api/v1/protected-environments", map[string]any{
		"repo": repo, "name": "production", "required_approvals": 2, "approver_roles": []string{"owner"},
		"allow_self_approval": true, "required_group": "release-managers", "required_group_approvals": 3,
	}), 400, "group minimum above total")
	h.expect(h.do(admin, "POST", "/api/v1/protected-environments", map[string]any{
		"repo": repo, "name": "production", "required_approvals": 2, "approver_roles": []string{"owner"},
		"allow_self_approval": true, "required_group": "release-managers", "required_group_approvals": 1,
	}), 200, "save rule")
	h.expect(h.do(admin, "DELETE", "/api/v1/approver-groups/release-managers", nil), 409, "delete group in use")

	yml := "stages: [d]\njobs:\n  ship:\n    stage: d\n    environment: production\n    script: [x]\n"
	p := h.seedPipeline(repo, "main", yml)
	if _, err := h.st.PromoteReadyJobs(ctx); err != nil {
		t.Fatal(err)
	}
	ship := h.jobID(p.ID, "ship")
	status := func() string {
		j, err := h.st.GetJob(ctx, ship)
		if err != nil {
			t.Fatal(err)
		}
		return j.Status
	}
	if status() != "blocked" {
		t.Fatalf("ship = %s, want blocked", status())
	}
	vote := func(who string) int {
		return h.do(h.login(who), "POST", "/api/v1/jobs/"+pid(ship)+"/approvals",
			proto.ApprovalRequest{Verdict: "approved"}).Code
	}
	if c := vote("dev@acme.io"); c != 403 {
		t.Errorf("developer vote = %d, want 403", c)
	}
	if c := vote("o1@acme.io"); c != 200 {
		t.Fatalf("owner 1 vote = %d", c)
	}
	if c := vote("o2@acme.io"); c != 200 {
		t.Fatalf("owner 2 vote = %d", c)
	}
	if status() != "blocked" {
		t.Fatalf("after two owners (none from the group): %s, want still blocked", status())
	}
	j, _ := h.st.GetJob(ctx, ship)
	if j.RequiredGroup != "release-managers" || j.RequiredGroupApprovals != 1 {
		t.Errorf("job gate = %q/%d", j.RequiredGroup, j.RequiredGroupApprovals)
	}
	if c := vote("HARISH@acme.io"); c != 200 {
		t.Fatalf("release manager vote = %d, want 200 (group members may vote)", c)
	}
	if status() != "pending" {
		t.Errorf("after a release manager approved: %s, want pending", status())
	}
}
