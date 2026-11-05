package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/review"
	"github.com/priytamjeepandey/forge-ci/internal/store"
	"github.com/priytamjeepandey/forge-ci/internal/vcs"
)

// TestReviewAndMergeAPI drives Level 1 through the API in open bootstrap
// mode: a rule, a Forge approval, the gate, a merge request.
func TestReviewAndMergeAPI(t *testing.T) {
	h := newReviewHarness(t)
	open := testCaller{}
	h.expect(h.do(open, http.MethodPut, "/api/v1/merge-policies",
		map[string]any{"repo": "acme/shop", "branch_pattern": "main", "ticket_pattern": "("}), http.StatusBadRequest, "bad pattern")
	rec := h.do(open, http.MethodPut, "/api/v1/merge-policies", map[string]any{"repo": "acme/shop", "branch_pattern": "main",
		"required_approvals": 1, "require_ci": false})
	h.expect(rec, http.StatusOK, "put rule")
	var p store.MergePolicy
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	if !p.DismissOnPush || !p.BlockOnChangesRequested || len(p.ApproverRoles) != 3 {
		t.Errorf("omitted fields did not take the safe defaults: %+v", p)
	}
	id := h.seedChange() // has a GitHub approval from an unlinked login
	path := "/api/v1/changes/" + strconv.FormatInt(id, 10)
	svc := review.New(h.st)
	svc.EvaluateGates(context.Background())

	var detail struct {
		Change      store.ChangeDetail `json:"change"`
		Gate        *store.GateResult  `json:"gate"`
		Permissions map[string]bool    `json:"permissions"`
	}
	_ = json.Unmarshal(h.do(open, http.MethodGet, path, nil).Body.Bytes(), &detail)
	head := detail.Change.HeadSHA
	// Bootstrap repo: any approval counts, the unlinked GitHub one included.
	if detail.Gate == nil || detail.Gate.State != "pass" || !detail.Permissions["can_merge"] || !detail.Permissions["can_review"] {
		t.Fatalf("detail = %+v %+v", detail.Gate, detail.Permissions)
	}

	h.expect(h.do(open, http.MethodPost, path+"/reviews", map[string]any{"verdict": "approve"}), http.StatusBadRequest, "no reviewer")
	h.expect(h.do(open, http.MethodPost, path+"/reviews", map[string]any{"verdict": "lgtm", "reviewer": "a@x.io"}), http.StatusBadRequest, "bad verdict")
	h.expect(h.do(open, http.MethodPost, path+"/reviews", map[string]any{"verdict": "request_changes", "reviewer": "carol@acme.io", "body": "needs a test"}),
		http.StatusOK, "request changes")
	svc.EvaluateGates(context.Background())
	h.expect(h.do(open, http.MethodPost, path+"/merge", map[string]any{"expected_head": head}), http.StatusConflict, "merge while changes requested")
	h.expect(h.do(open, http.MethodPost, path+"/reviews", map[string]any{"verdict": "dismiss", "reviewer": "carol@acme.io"}), http.StatusOK, "withdraw")
	svc.EvaluateGates(context.Background())

	h.expect(h.do(open, http.MethodPost, path+"/merge", map[string]any{"expected_head": "stale"}), http.StatusConflict, "stale head")
	h.expect(h.do(open, http.MethodPost, path+"/merge", map[string]any{"expected_head": head, "method": "octopus"}), http.StatusBadRequest, "bad method")
	rec = h.do(open, http.MethodPost, path+"/merge", map[string]any{"expected_head": head, "method": "squash"})
	h.expect(rec, http.StatusAccepted, "merge")
	var first struct {
		Action store.OutboxItem `json:"action"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &first)
	rec = h.do(open, http.MethodPost, path+"/merge", map[string]any{"expected_head": head, "method": "squash"})
	var second struct {
		Action store.OutboxItem `json:"action"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &second)
	if first.Action.ID == 0 || second.Action.ID != first.Action.ID || first.Action.Class != vcs.ClassUser {
		t.Errorf("double click queued two merges: %+v / %+v", first.Action, second.Action)
	}
	// The page lists the queued merge.
	var withActions struct {
		Actions []store.OutboxItem `json:"actions"`
	}
	_ = json.Unmarshal(h.do(open, http.MethodGet, path, nil).Body.Bytes(), &withActions)
	found := false
	for _, a := range withActions.Actions {
		found = found || a.Kind == review.KindMerge
	}
	if !found {
		t.Errorf("actions = %+v", withActions.Actions)
	}
}

// TestReviewPermissionsEnforced: with SSO on, only people with an approver
// role (or in the rule's group) may approve, never on their own change;
// merging needs a repo role; rules are admin-only.
func TestReviewPermissionsEnforced(t *testing.T) {
	h := newEnforcedHarness(t)
	ctx := context.Background()
	id, _, err := h.st.ApplyChange(ctx, "acme/shop", "github", &vcs.Change{ID: "5", Title: "x", Author: "dev-gh",
		SourceBranch: "f", TargetBranch: "main", State: "open", HeadSHA: "h1", UpdatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	for who, role := range map[string]string{"dev@acme.io": "developer", "owner@acme.io": "owner"} {
		if _, err := h.st.AddMember(ctx, "acme/shop", who, role); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.st.PutHostIdentity(ctx, store.HostIdentity{Provider: "github", Login: "dev-gh", Email: "dev@acme.io"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.UpsertMergePolicy(ctx, store.MergePolicy{Repo: "acme/shop", BranchPattern: "main", RequiredApprovals: 1,
		ApproverRoles: []string{"owner"}}); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/changes/" + strconv.FormatInt(id, 10)
	dev, owner, outsider, admin := h.login("dev@acme.io"), h.login("owner@acme.io"), h.login("eve@else.io"), h.login("admin@acme.io")

	h.expect(h.do(dev, http.MethodPost, path+"/reviews", map[string]any{"verdict": "approve"}), http.StatusForbidden, "developer under an owner-only rule")
	h.expect(h.do(outsider, http.MethodPost, path+"/reviews", map[string]any{"verdict": "approve", "reviewer": "owner@acme.io"}),
		http.StatusForbidden, "outsider claiming another identity")
	h.expect(h.do(owner, http.MethodPost, path+"/reviews", map[string]any{"verdict": "approve"}), http.StatusOK, "owner approves")
	h.expect(h.do(outsider, http.MethodPost, path+"/merge", map[string]any{"expected_head": "h1"}), http.StatusForbidden, "outsider merges")

	// The author may not approve their own change even with the role.
	if _, err := h.st.UpsertMergePolicy(ctx, store.MergePolicy{Repo: "acme/shop", BranchPattern: "main", RequiredApprovals: 1,
		ApproverRoles: []string{"owner", "developer"}}); err != nil {
		t.Fatal(err)
	}
	rec := h.do(dev, http.MethodPost, path+"/reviews", map[string]any{"verdict": "approve"})
	h.expect(rec, http.StatusForbidden, "self approval")

	h.expect(h.do(owner, http.MethodPut, "/api/v1/merge-policies", map[string]any{"repo": "acme/shop"}), http.StatusForbidden, "non-admin edits rules")
	h.expect(h.do(owner, http.MethodPut, "/api/v1/host-identities", map[string]any{"provider": "github", "login": "x", "email": "x@y.z"}),
		http.StatusForbidden, "non-admin links identities")
	h.expect(h.do(admin, http.MethodPut, "/api/v1/host-identities", map[string]any{"provider": "nope", "login": "x", "email": "x@y.z"}),
		http.StatusBadRequest, "unknown provider")
	h.expect(h.do(admin, http.MethodDelete, "/api/v1/host-identities/github/dev-gh", nil), http.StatusNoContent, "unlink")

	var reviews store.ChangeDetail
	var d struct {
		Change store.ChangeDetail `json:"change"`
	}
	_ = json.Unmarshal(h.do(owner, http.MethodGet, path, nil).Body.Bytes(), &d)
	reviews = d.Change
	if len(reviews.Reviews) != 1 || reviews.Reviews[0].Reviewer != "owner@acme.io" || reviews.Reviews[0].Source != "forge" {
		t.Errorf("reviews = %+v", reviews.Reviews)
	}
}
