package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

const prodYAML = `
stages: [deploy]
jobs:
  ship:
    stage: deploy
    script: [echo ship]
    environment: production
`

// blockedProdJob seeds a production deploy and parks it at the approval gate.
func (h *apiHarness) blockedProdJob(repo string) int64 {
	h.t.Helper()
	p := h.seedPipeline(repo, "main", prodYAML)
	if _, err := h.st.PromoteReadyJobs(context.Background()); err != nil {
		h.t.Fatalf("PromoteReadyJobs: %v", err)
	}
	id := h.jobID(p.ID, "ship")
	var status string
	_ = h.db.QueryRow(context.Background(), `SELECT status FROM jobs WHERE id=$1`, id).Scan(&status)
	if status != "blocked" {
		h.t.Fatalf("ship status = %q, want blocked", status)
	}
	return id
}

func (h *apiHarness) canApprove(c testCaller, jobID int64, query string) bool {
	h.t.Helper()
	rec := h.do(c, "GET", "/api/v1/jobs/"+pid(jobID)+"/approval-permission"+query, nil)
	h.expect(rec, 200, "approval-permission")
	var out struct {
		CanApprove bool `json:"can_approve"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return out.CanApprove
}

// TestApprovalRolesEnforced: with SSO, a developer is refused when the rule's
// approver_roles leave developers out — whatever name they put in the body —
// and an owner succeeds. The UI hint agrees with both outcomes.
func TestApprovalRolesEnforced(t *testing.T) {
	h := newEnforcedHarness(t)
	ctx := context.Background()
	const repo = "acme/prod-gate"
	for user, role := range map[string]string{"dev@acme.io": "developer", "owner@acme.io": "owner"} {
		if _, err := h.st.AddMember(ctx, repo, user, role); err != nil {
			t.Fatal(err)
		}
	}
	h.expect(h.do(h.login("admin@acme.io"), "POST", "/api/v1/protected-environments", proto.ProtectedEnvironment{
		Repo: repo, Name: "production", RequiredApprovals: 1,
		ApproverRoles: []string{"admin", "owner"}, AllowSelfApproval: false,
	}), 200, "save rule")
	job := h.blockedProdJob(repo)
	dev, owner := h.login("dev@acme.io"), h.login("owner@acme.io")

	if h.canApprove(dev, job, "") {
		t.Error("approval-permission says the developer can approve")
	}
	h.expect(h.do(dev, "POST", "/api/v1/jobs/"+pid(job)+"/approvals",
		map[string]string{"approver": "owner@acme.io", "verdict": "approved"}), 403, "developer approve")

	if !h.canApprove(owner, job, "") {
		t.Error("approval-permission says the owner cannot approve")
	}
	h.expect(h.do(owner, "POST", "/api/v1/jobs/"+pid(job)+"/approvals",
		map[string]string{"verdict": "approved"}), 200, "owner approve")
}

// TestApprovalMemberlessRepo: a repo with no members used to skip the role
// check, so any signed-in user could approve production. Now only a platform
// admin (counted as "admin") may.
func TestApprovalMemberlessRepo(t *testing.T) {
	h := newEnforcedHarness(t)
	job := h.blockedProdJob("acme/no-members")
	h.expect(h.do(h.login("dev@acme.io"), "POST", "/api/v1/jobs/"+pid(job)+"/approvals",
		map[string]string{"verdict": "approved"}), 403, "signed-in non-member approve")
	h.expect(h.do(h.login("admin@acme.io"), "POST", "/api/v1/jobs/"+pid(job)+"/approvals",
		map[string]string{"verdict": "approved"}), 200, "platform admin approve")
}

// TestApprovalOpenMode: without SSO every caller passes isAdmin, which must not
// make them an approver. The asserted name has to be a member with an allowed
// role.
func TestApprovalOpenMode(t *testing.T) {
	st, db := newCFRStore(t, "")
	t.Setenv("ADMIN_EMAILS", "admin@acme.io")
	h := &apiHarness{t: t, st: st, db: db, srv: New(st, nil, nil)}
	const repo = "acme/open-gate"
	if _, err := st.AddMember(context.Background(), repo, "owner@acme.io", "owner"); err != nil {
		t.Fatal(err)
	}
	job := h.blockedProdJob(repo)
	anon := testCaller{}

	// Before a name is typed the hint can't know; with one it can.
	if !h.canApprove(anon, job, "") {
		t.Error("open-mode hint without a name should defer to submit")
	}
	if h.canApprove(anon, job, "?approver=dev@acme.io") {
		t.Error("open-mode hint says a non-member can approve")
	}
	for _, name := range []string{"dev@acme.io", "admin@acme.io"} {
		h.expect(h.do(anon, "POST", "/api/v1/jobs/"+pid(job)+"/approvals",
			map[string]string{"approver": name, "verdict": "approved"}), 403, "open-mode "+name)
	}
	h.expect(h.do(anon, "POST", "/api/v1/jobs/"+pid(job)+"/approvals",
		map[string]string{"approver": "owner@acme.io", "verdict": "approved"}), 200, "open-mode owner")
}

// Approver roles must be real roles: a typo such as "maintainer" would
// otherwise save a rule nobody can satisfy.
func TestProtectedEnvRejectsUnknownRole(t *testing.T) {
	h := newEnforcedHarness(t)
	h.expect(h.do(h.login("admin@acme.io"), "POST", "/api/v1/protected-environments", proto.ProtectedEnvironment{
		Repo: "acme/x", Name: "production", ApproverRoles: []string{"admin", "maintainer"},
	}), 400, "unknown role")
}
