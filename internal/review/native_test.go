package review

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/blob"
	"github.com/priytamjeepandey/forge-ci/internal/mirror"
	"github.com/priytamjeepandey/forge-ci/internal/proto"
	"github.com/priytamjeepandey/forge-ci/internal/store"
	"github.com/priytamjeepandey/forge-ci/internal/vcs"
	"github.com/priytamjeepandey/forge-ci/internal/vcs/vcstest"
)

const plainRepo = "acme/plain"

// gitHarness connects a plain git server (no pull requests) to Forge.
func newGitHarness(t *testing.T) (*harness, *vcstest.FakeGit) {
	t.Helper()
	if !mirror.MergeSupported() {
		t.Skip("git 2.38+ not installed")
	}
	g := vcstest.NewFakeGit(t)
	h := newHarnessOn(t, g, proto.RepoRegistration{})
	h.root, h.blobs = t.TempDir(), blob.NewLocal(t.TempDir())
	return h, g
}

// runTasks plays a runner: every queued task is done with the real mirror
// code, and reported as the runner would. Returns how many ran.
func (h *harness) runTasks() int {
	h.t.Helper()
	n := 0
	for ; n < 20; n++ {
		t, err := h.st.ClaimRunnerTask(h.ctx, "runner-1", []string{proto.TaskKindDiff, proto.TaskKindMerge}, 0, time.Minute)
		if err != nil {
			h.t.Fatal(err)
		}
		if t == nil {
			return n
		}
		cloneURL, redact, _, err := h.st.TaskCloneURL(h.ctx, t.Repo)
		if err != nil {
			h.t.Fatal(err)
		}
		m := mirror.Open(h.root, t.Repo)
		failure := ""
		switch t.Kind {
		case proto.TaskKindDiff:
			k, d, _ := store.DiffKeyFromTask(t)
			res, err := m.Diff(h.ctx, cloneURL, redact, d)
			if err == nil {
				err = StoreDiff(h.ctx, h.st, h.blobs, k, res)
			}
			if err != nil {
				failure = err.Error()
			}
		case proto.TaskKindMerge:
			task, _ := store.MergeTaskFromRow(t)
			res, err := m.Merge(h.ctx, cloneURL, redact, task)
			if err == nil {
				err = h.st.SetRunnerTaskResult(h.ctx, t.ID, "runner-1", res)
			}
			if err != nil {
				failure = err.Error()
			}
		}
		if _, _, err := h.st.FinishRunnerTask(h.ctx, t.ID, "runner-1", failure); err != nil {
			h.t.Fatal(err)
		}
	}
	return n
}

// pump runs the workers (and a runner) the way the server's tick does, with
// waiting writes made due at once.
func (h *harness) pump() {
	h.t.Helper()
	for i := 0; i < 8; i++ {
		h.svc.RefreshDue(h.ctx)
		h.svc.EvaluateGates(h.ctx)
		h.svc.AdvanceQueues(h.ctx)
		if _, err := h.db.Exec(h.ctx, `UPDATE provider_outbox SET next_attempt_at = now() WHERE state = 'pending'`); err != nil {
			h.t.Fatal(err)
		}
		h.svc.SendOutbox(h.ctx)
		h.runTasks()
	}
}

// native opens a Forge-native change from branch into main and syncs it.
func (h *harness) native(branch, title string) int64 {
	h.t.Helper()
	id, number, err := h.st.CreateNativeChange(h.ctx, store.NativeChange{Repo: plainRepo, Provider: "git",
		Author: "dev@acme.io", Title: title, SourceBranch: branch, TargetBranch: "main"})
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.st.RequestChangeRefresh(h.ctx, plainRepo, "git", number, store.PriorityWebhook, 0, false); err != nil {
		h.t.Fatal(err)
	}
	h.svc.RefreshDue(h.ctx)
	return id
}

func (h *harness) rule(changeID int64, id string) store.GateRule {
	h.t.Helper()
	g, err := h.st.GetGate(h.ctx, changeID)
	if err != nil || g == nil {
		h.t.Fatalf("no gate for change %d (%v)", changeID, err)
	}
	for _, r := range g.Rules {
		if r.ID == id {
			return r
		}
	}
	h.t.Fatalf("gate has no %q rule: %+v", id, g.Rules)
	return store.GateRule{}
}

func (h *harness) policy(p store.MergePolicy) {
	h.t.Helper()
	p.Repo, p.BranchPattern = plainRepo, "main"
	if _, err := h.st.UpsertMergePolicy(h.ctx, p); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) queueMerge(changeID int64, method string) store.OutboxItem {
	h.t.Helper()
	c, _ := h.st.ChangeForAction(h.ctx, changeID)
	payload, _ := json.Marshal(MergePayload{Method: method, ExpectedHead: c.HeadSHA})
	o, err := h.st.EnqueueOutbox(h.ctx, store.OutboxItem{IdemKey: "merge:" + itoa(changeID), Kind: KindMerge, Repo: plainRepo,
		ChangeID: &changeID, Class: vcs.ClassUser, Payload: payload, RequestedBy: "lead@acme.io"}, "")
	if err != nil {
		h.t.Fatal(err)
	}
	return *o
}

// TestNativeChangeMergedByRunner is phase F's acceptance test for plain
// git: a change opened in Forge reads its branches, passes the gate, and a
// runner merges exactly the approved head into main — no host API anywhere.
func TestNativeChangeMergedByRunner(t *testing.T) {
	h, g := newGitHarness(t)
	head := g.CommitFile(t, "feature", "feature.txt", "seat lock\n")
	mainBefore := g.Tip(t, "main")
	h.policy(store.MergePolicy{RequiredApprovals: 1, DismissOnPush: true})
	id := h.native("feature", "Seat lock")
	d, err := h.st.GetChangeDetail(h.ctx, id)
	if err != nil || d.HeadSHA != head || d.BaseSHA != mainBefore || d.State != "open" || d.Number != "1" {
		t.Fatalf("native change after its first refresh = %+v, %v", d, err)
	}
	h.settle()
	if r := h.rule(id, "approvals"); r.State != "pending" {
		t.Fatalf("approvals before review = %+v", r)
	}
	h.approve(id, "lead@acme.io")
	h.settle()

	o := h.queueMerge(id, "squash")
	h.pump()
	acts, _ := h.st.ChangeActions(h.ctx, id, 1)
	if acts[0].ID != o.ID || acts[0].State != "done" {
		t.Fatalf("merge = %+v", acts[0])
	}
	var res map[string]string
	_ = json.Unmarshal(acts[0].Result, &res)
	tip := g.Tip(t, "main")
	if tip == mainBefore || res["merge_sha"] != tip {
		t.Fatalf("main = %s, merge reported %q", tip, res["merge_sha"])
	}
	if got := g.Show(t, tip+":feature.txt"); got != "seat lock" {
		t.Errorf("merged main has feature.txt = %q", got)
	}
	if parents := strings.Fields(g.Show(t, "-s", "--format=%P", tip)); len(parents) != 1 || parents[0] != mainBefore {
		t.Errorf("squash commit parents = %v, want [%s]", parents, mainBefore)
	}
	if subj := g.Show(t, "-s", "--format=%s%n%ae", tip); subj != "Seat lock (#1)\nlead@acme.io" {
		t.Errorf("squash commit = %q", subj)
	}
	if d, _ := h.st.GetChangeDetail(h.ctx, id); d.State != "merged" {
		t.Errorf("Forge's copy = %s, want merged", d.State)
	}

	// A later push to the branch does not reopen or move a merged change.
	g.CommitFile(t, "feature", "later.txt", "later\n")
	_ = h.st.RequestChangeRefresh(h.ctx, plainRepo, "git", "1", store.PriorityWebhook, 0, false)
	h.svc.RefreshDue(h.ctx)
	if d, _ := h.st.GetChangeDetail(h.ctx, id); d.State != "merged" || d.HeadSHA != head {
		t.Errorf("after a later push = %s %s", d.State, d.HeadSHA)
	}
	// The same branches can be proposed again once nothing open uses them.
	if _, _, err := h.st.CreateNativeChange(h.ctx, store.NativeChange{Repo: plainRepo, Provider: "git", Author: "dev@acme.io",
		Title: "Follow-up", SourceBranch: "feature", TargetBranch: "main"}); err != nil {
		t.Errorf("a new change for merged branches: %v", err)
	}
}

func TestNativeMergeRefusesConflictsAndMovedHeads(t *testing.T) {
	h, g := newGitHarness(t)
	g.CommitFile(t, "feature", "shared.txt", "feature side\n")
	g.CommitFile(t, "main", "shared.txt", "main side\n")
	id := h.native("feature", "Conflicting")
	if _, _, err := h.st.CreateNativeChange(h.ctx, store.NativeChange{Repo: plainRepo, Provider: "git", Author: "dev@acme.io",
		Title: "Again", SourceBranch: "feature", TargetBranch: "main"}); err != store.ErrDuplicateChange {
		t.Errorf("duplicate open change: %v", err)
	}
	mainBefore := g.Tip(t, "main")
	h.queueMerge(id, "merge")
	h.pump()
	acts, _ := h.st.ChangeActions(h.ctx, id, 1)
	if acts[0].State != "failed" || !strings.Contains(acts[0].LastError, "conflicts in shared.txt") {
		t.Fatalf("conflicting merge = %+v", acts[0])
	}
	if g.Tip(t, "main") != mainBefore {
		t.Fatal("a conflicting merge pushed to main")
	}

	// New commits after the merge was asked for: the runner refuses.
	id2 := h.native("other", "Other")
	g.CommitFile(t, "other", "o.txt", "1\n")
	_ = h.st.RequestChangeRefresh(h.ctx, plainRepo, "git", "2", store.PriorityWebhook, 0, false)
	h.svc.RefreshDue(h.ctx)
	h.queueMerge(id2, "merge")
	g.CommitFile(t, "other", "o.txt", "2\n") // pushed after the click
	h.pump()
	acts, _ = h.st.ChangeActions(h.ctx, id2, 1)
	if acts[0].State != "failed" || !strings.Contains(acts[0].LastError, "new commits") {
		t.Fatalf("merge of a moved branch = %+v", acts[0])
	}
}

// TestCodeOwnersRule: CODEOWNERS on the target names who must approve which
// touched files; approvals from others do not count for them.
func TestCodeOwnersRule(t *testing.T) {
	h, g := newGitHarness(t)
	g.CommitFile(t, "main", ".github/CODEOWNERS", "*.go @acme/backend\n/docs/ lead@acme.io\nREADME.md\n")
	g.CommitFile(t, "feature", "app.go", "package app\n")
	g.CommitFile(t, "feature", "docs/guide.md", "# guide\n")
	if _, err := h.st.UpsertApproverGroup(h.ctx, store.ApproverGroup{Name: "backend", Members: []string{"Be@acme.io"}}); err != nil {
		t.Fatal(err)
	}
	h.policy(store.MergePolicy{RequiredApprovals: 1, RequireCodeOwners: true})
	id := h.native("feature", "Guide and app")
	h.settle()
	if r := h.rule(id, "code_owners"); r.State != "pending" || !strings.Contains(r.Detail, "Working out") {
		t.Fatalf("before the diff = %+v", r)
	}
	h.pump() // the runner computes the diff, which reads CODEOWNERS
	r := h.rule(id, "code_owners")
	if r.State != "pending" || !strings.Contains(r.Detail, "@acme/backend for app.go") || !strings.Contains(r.Detail, "lead@acme.io for docs/guide.md") {
		t.Fatalf("owners wanted = %+v", r)
	}
	h.approve(id, "someone@acme.io") // counts for approvals, not for owners
	h.approve(id, "be@acme.io")
	h.settle()
	if r := h.rule(id, "code_owners"); r.State != "pending" || strings.Contains(r.Detail, "app.go") {
		t.Fatalf("after backend approved = %+v", r)
	}
	h.approve(id, "lead@acme.io")
	h.settle()
	if r := h.rule(id, "code_owners"); r.State != "pass" || r.Detail != "Code owners approved all 2 owned files" {
		t.Fatalf("after both owners approved = %+v", r)
	}
}

// pipelineStub stands in for the API's pipeline creation.
type pipelineStub struct {
	h   *harness
	ids []int64
	ref []string
	sha []string
}

func (p *pipelineStub) start(ctx context.Context, repo, ref, sha, actor string) (int64, error) {
	pl, err := p.h.st.CreatePipeline(ctx, proto.CreatePipelineRequest{Repo: repo, Ref: ref, SHA: sha, Config: "test: {}",
		Source: "merge_queue", TriggeredBy: actor}, nil, nil, false, false)
	if err != nil {
		return 0, err
	}
	if _, err := p.h.db.Exec(ctx, `UPDATE pipelines SET status = 'running' WHERE id = $1`, pl.ID); err != nil {
		return 0, err
	}
	p.ids, p.ref, p.sha = append(p.ids, pl.ID), append(p.ref, ref), append(p.sha, sha)
	return pl.ID, nil
}

func (p *pipelineStub) finish(i int, status string) {
	p.h.t.Helper()
	if _, err := p.h.db.Exec(p.h.ctx, `UPDATE pipelines SET status = $2 WHERE id = $1`, p.ids[i], status); err != nil {
		p.h.t.Fatal(err)
	}
}

func (h *harness) queued(changeID int64) {
	h.t.Helper()
	c, _ := h.st.ChangeForAction(h.ctx, changeID)
	if _, err := h.st.EnqueueMerge(h.ctx, store.QueueEntry{ChangeID: changeID, Repo: plainRepo, TargetBranch: "main",
		HeadSHA: c.HeadSHA, Method: "merge", RequestedBy: "lead@acme.io"}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) entry(changeID int64) *store.QueueEntry {
	h.t.Helper()
	e, err := h.st.LatestQueueEntry(h.ctx, changeID)
	if err != nil || e == nil {
		h.t.Fatalf("no queue entry for %d (%v)", changeID, err)
	}
	return e
}

// TestMergeQueue is phase F's merge queue acceptance test: each change is
// tested merged onto the latest main, in order, and merged only when that
// passes; the gate (and so the host's Merge button) waits for the queue.
func TestMergeQueue(t *testing.T) {
	h, g := newGitHarness(t)
	stub := &pipelineStub{h: h}
	h.svc.SetPipelineStarter(stub.start)
	g.CommitFile(t, "a", "a.txt", "a\n")
	g.CommitFile(t, "b", "b.txt", "b\n")
	h.policy(store.MergePolicy{RequiredApprovals: 1, MergeQueue: true})
	a, b := h.native("a", "A"), h.native("b", "B")
	h.approve(a, "lead@acme.io")
	h.approve(b, "lead@acme.io")
	h.settle()
	if r := h.rule(a, "merge_queue"); r.State != "pending" || !strings.Contains(r.Detail, "Add the change") {
		t.Fatalf("queue rule before queueing = %+v", r)
	}
	h.queued(a)
	h.queued(b)
	h.settle()
	if r := h.rule(b, "merge_queue"); r.Detail != "In the merge queue, position 2" {
		t.Fatalf("second in line = %+v", r)
	}
	main0 := g.Tip(t, "main")

	// A is merged onto main and tested on forge-queue/main; main is untouched.
	h.pump()
	if len(stub.ids) != 1 || stub.ref[0] != "forge-queue/main" || g.Tip(t, "forge-queue/main") != stub.sha[0] {
		t.Fatalf("test pipelines = %v %v", stub.ref, stub.sha)
	}
	if e := h.entry(a); e.State != "testing" || e.BaseSHA != main0 || g.Tip(t, "main") != main0 {
		t.Fatalf("A while testing = %+v", e)
	}
	if e := h.entry(b); e.State != "queued" {
		t.Fatalf("B must wait its turn: %+v", e)
	}
	if r := h.rule(a, "merge_queue"); r.State != "pending" || !strings.Contains(r.Detail, "Testing on top of main") {
		t.Fatalf("A's queue rule while testing = %+v", r)
	}

	// A's test passes: the gate passes and A merges.
	stub.finish(0, "success")
	h.pump()
	if e := h.entry(a); e.State != "merged" {
		t.Fatalf("A after its test passed = %+v", e)
	}
	main1 := g.Tip(t, "main")
	if main1 == main0 || g.Show(t, main1+":a.txt") != "a" {
		t.Fatal("A is not on main")
	}

	// B is tested on top of the new main (with A), and its failure keeps it out.
	if len(stub.ids) != 2 {
		t.Fatalf("B's test did not start: %v", stub.ids)
	}
	if e := h.entry(b); e.BaseSHA != main1 {
		t.Fatalf("B was tested on %s, want the main with A (%s)", e.BaseSHA, main1)
	}
	stub.finish(1, "failed")
	h.pump()
	if e := h.entry(b); e.State != "failed" || !strings.Contains(e.Error, "failed") {
		t.Fatalf("B after its test failed = %+v", e)
	}
	if g.Tip(t, "main") != main1 {
		t.Fatal("a change whose test failed reached main")
	}
	if r := h.rule(b, "merge_queue"); r.State != "pending" {
		t.Fatalf("B's queue rule after leaving the queue = %+v", r)
	}
}

func TestQueueDropsChangesThatMove(t *testing.T) {
	h, g := newGitHarness(t)
	h.svc.SetPipelineStarter((&pipelineStub{h: h}).start)
	g.CommitFile(t, "a", "a.txt", "a\n")
	h.policy(store.MergePolicy{RequiredApprovals: 0, MergeQueue: true})
	a := h.native("a", "A")
	h.settle()
	h.queued(a)
	g.CommitFile(t, "a", "a.txt", "a2\n")
	_ = h.st.RequestChangeRefresh(h.ctx, plainRepo, "git", "1", store.PriorityWebhook, 0, false)
	h.pump()
	if e := h.entry(a); e.State != "failed" || !strings.Contains(e.Error, "new commits") {
		t.Fatalf("moved change in the queue = %+v", e)
	}
	// Closing a queued change takes it out.
	h.queued(a)
	closed := "closed"
	if err := h.st.EditNativeChange(h.ctx, a, store.NativeEdit{State: &closed}); err != nil {
		t.Fatal(err)
	}
	if e := h.entry(a); e.State != "removed" {
		t.Fatalf("closed change = %+v", e)
	}
}
