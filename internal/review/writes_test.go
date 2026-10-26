package review

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/priytamjeepandey/forge-ci/internal/store"
	"github.com/priytamjeepandey/forge-ci/internal/vcs"
)

// gated opens a pull request on the fake host (with branch protection
// requiring forge/merge-gate), a one-approval rule for main, and syncs it.
func (h *harness) gated(title string) (hostID string, changeID int64) {
	h.t.Helper()
	h.gh.Protect(vcs.GateContext)
	if _, err := h.st.UpsertMergePolicy(h.ctx, store.MergePolicy{Repo: vcstestRepo, BranchPattern: "main",
		RequiredApprovals: 1, DismissOnPush: true, BlockOnChangesRequested: true}); err != nil {
		h.t.Fatal(err)
	}
	id, _ := h.gh.Open(h.t, title)
	h.ingest(id, "change")
	h.svc.RefreshDue(h.ctx)
	list := h.open()
	for _, c := range list {
		if c.Number == id {
			return id, c.ID
		}
	}
	h.t.Fatalf("change %s not synced: %+v", id, list)
	return "", 0
}

const vcstestRepo = "acme/shop"

// settle runs gate evaluation and the outbox until nothing is left to do now.
func (h *harness) settle() {
	h.t.Helper()
	for i := 0; i < 10; i++ {
		if h.svc.EvaluateGates(h.ctx) == 0 && h.svc.SendOutbox(h.ctx) == 0 {
			return
		}
	}
}

func (h *harness) approve(changeID int64, who string) {
	h.t.Helper()
	c, err := h.st.ChangeForAction(h.ctx, changeID)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.st.SetForgeReview(h.ctx, changeID, who, "approve", "", c.HeadSHA); err != nil {
		h.t.Fatal(err)
	}
}

// TestMergeOnHostBlockedUntilGatePasses is phase B's acceptance test: with
// forge/merge-gate required on the host, clicking Merge on GitHub fails until
// Forge's rules pass, then succeeds.
func TestMergeOnHostBlockedUntilGatePasses(t *testing.T) {
	h := newHarness(t)
	id, changeID := h.gated("Seat lock")
	h.settle()
	head := h.gh.Head(t, id)
	state, desc, link := h.gh.StatusOf(head, vcs.GateContext)
	if state != "pending" || desc != "0 of 1 approvals" || !strings.HasSuffix(link, "/changes/"+itoa(changeID)) {
		t.Fatalf("published gate = %q %q %q", state, desc, link)
	}
	if err := h.gh.HostMerge(t, id); err == nil {
		t.Fatal("GitHub merged while Forge's gate was pending")
	}
	h.approve(changeID, "ashwin@acme.io")
	h.settle()
	if state, desc, _ := h.gh.StatusOf(head, vcs.GateContext); state != "success" || desc != "All 2 merge rules pass" {
		t.Fatalf("gate after approval = %q %q", state, desc)
	}
	if g, _ := h.st.GetGate(h.ctx, changeID); g == nil || g.PublishedState != "pass" {
		t.Errorf("published state not recorded: %+v", g)
	}
	if err := h.gh.HostMerge(t, id); err != nil {
		t.Errorf("GitHub refused the merge after the gate passed: %v", err)
	}
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// TestMergeFromForge: the merge waits for its passing gate to reach the
// host, then merges exactly the approved head.
func TestMergeFromForge(t *testing.T) {
	h := newHarness(t)
	id, changeID := h.gated("Merge from Forge")
	h.approve(changeID, "ashwin@acme.io")
	if h.svc.EvaluateGates(h.ctx) != 1 {
		t.Fatal("gate not evaluated")
	}
	head := h.gh.Head(t, id)
	payload, _ := json.Marshal(MergePayload{Method: "squash", ExpectedHead: head})
	cid := changeID
	o, err := h.st.EnqueueOutbox(h.ctx, store.OutboxItem{IdemKey: "merge:test", Kind: KindMerge, Repo: vcstestRepo,
		ChangeID: &cid, Class: vcs.ClassUser, Payload: payload, RequestedBy: "ashwin@acme.io"}, "")
	if err != nil {
		t.Fatal(err)
	}
	// One tick: the merge (user class, claimed first) finds the gate not yet
	// on the host and waits without spending an attempt; the publish goes.
	h.svc.SendOutbox(h.ctx)
	if h.gh.State(t, id) != "open" {
		t.Fatal("merged before the gate reached the host")
	}
	if _, err := h.db.Exec(h.ctx, `UPDATE provider_outbox SET next_attempt_at = now() WHERE id = $1`, o.ID); err != nil {
		t.Fatal(err)
	}
	h.svc.SendOutbox(h.ctx)
	if h.gh.State(t, id) != "merged" {
		t.Fatalf("not merged on the host: %s", h.gh.State(t, id))
	}
	acts, _ := h.st.ChangeActions(h.ctx, changeID, 10)
	if acts[0].Kind != KindMerge || acts[0].State != "done" || acts[0].Attempts != 1 || !strings.Contains(string(acts[0].Result), "merge_sha") {
		t.Errorf("merge action = %+v", acts[0])
	}
	h.svc.RefreshDue(h.ctx)
	if d, _ := h.st.GetChangeDetail(h.ctx, changeID); d.State != "merged" {
		t.Errorf("Forge's copy = %s, want merged", d.State)
	}
	// A double click is the same request, not a second merge.
	again, _ := h.st.EnqueueOutbox(h.ctx, store.OutboxItem{IdemKey: "merge:test", Kind: KindMerge, Repo: vcstestRepo,
		ChangeID: &cid, Class: vcs.ClassUser, Payload: payload}, "")
	if again.ID != o.ID || again.State != "done" {
		t.Errorf("repeat enqueue = %+v", again)
	}
}

// TestMergeRefusesAMovedHead: someone pushed after approval — the host says
// so (409) and Forge does not retry.
func TestMergeRefusesAMovedHead(t *testing.T) {
	h := newHarness(t)
	id, changeID := h.gated("Moving target")
	h.approve(changeID, "ashwin@acme.io")
	h.settle()
	approved := h.gh.Head(t, id)
	h.gh.Push(t, id) // its webhook has not arrived yet
	payload, _ := json.Marshal(MergePayload{Method: "merge", ExpectedHead: approved})
	cid := changeID
	if _, err := h.st.EnqueueOutbox(h.ctx, store.OutboxItem{IdemKey: "merge:moved", Kind: KindMerge, Repo: vcstestRepo,
		ChangeID: &cid, Class: vcs.ClassUser, Payload: payload}, ""); err != nil {
		t.Fatal(err)
	}
	h.svc.SendOutbox(h.ctx)
	acts, _ := h.st.ChangeActions(h.ctx, changeID, 1)
	if acts[0].State != "failed" || !strings.Contains(acts[0].LastError, "new commits") {
		t.Errorf("merge of a moved head = %+v", acts[0])
	}
	if h.gh.State(t, id) != "open" {
		t.Error("merged a head nobody approved")
	}
}

// TestMergeRetryNeverMergesTwice: a merge whose response was lost (the host
// merged; Forge saw an error) is checked on the host before any resend.
func TestMergeRetryNeverMergesTwice(t *testing.T) {
	h := newHarness(t)
	id, changeID := h.gated("Lost response")
	h.approve(changeID, "ashwin@acme.io")
	h.settle()
	if err := h.gh.HostMerge(t, id); err != nil { // what the lost request did
		t.Fatal(err)
	}
	payload, _ := json.Marshal(MergePayload{Method: "merge", ExpectedHead: h.gh.Head(t, id)})
	cid := changeID
	o, _ := h.st.EnqueueOutbox(h.ctx, store.OutboxItem{IdemKey: "merge:lost", Kind: KindMerge, Repo: vcstestRepo,
		ChangeID: &cid, Class: vcs.ClassUser, Payload: payload}, "")
	if _, err := h.db.Exec(h.ctx, `UPDATE provider_outbox SET attempts = 1 WHERE id = $1`, o.ID); err != nil {
		t.Fatal(err)
	}
	h.svc.SendOutbox(h.ctx)
	acts, _ := h.st.ChangeActions(h.ctx, changeID, 1)
	if acts[0].State != "done" || !strings.Contains(string(acts[0].Result), "earlier attempt") {
		t.Errorf("retry = %+v", acts[0])
	}
}

// TestGatePublishesCoalesce: verdicts that change faster than the outbox
// sends publish only the newest; removing the rule releases the host.
func TestGatePublishesCoalesce(t *testing.T) {
	h := newHarness(t)
	id, changeID := h.gated("Busy")
	h.svc.EvaluateGates(h.ctx) // pending, queued
	h.approve(changeID, "ashwin@acme.io")
	h.svc.EvaluateGates(h.ctx) // pass, supersedes the unsent pending
	before := h.gh.Requests()
	h.settle()
	if got := h.gh.Requests() - before; got != 1 {
		t.Errorf("publishes sent = %d, want 1 (the newest)", got)
	}
	ps, _ := h.st.ListMergePolicies(h.ctx)
	if _, err := h.st.DeleteMergePolicy(h.ctx, ps[0].ID); err != nil {
		t.Fatal(err)
	}
	h.gh.Push(t, id)
	h.ingest(id, "change")
	h.svc.RefreshDue(h.ctx)
	h.settle()
	if state, desc, _ := h.gh.StatusOf(h.gh.Head(t, id), vcs.GateContext); state != "success" || !strings.Contains(desc, "No Forge merge rules") {
		t.Errorf("after the rule was removed: %q %q", state, desc)
	}
}
