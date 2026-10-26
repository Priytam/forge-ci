package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/priytamjeepandey/forge-ci/internal/blob"
	"github.com/priytamjeepandey/forge-ci/internal/mirror"
	"github.com/priytamjeepandey/forge-ci/internal/proto"
	"github.com/priytamjeepandey/forge-ci/internal/review"
	"github.com/priytamjeepandey/forge-ci/internal/store"
	"github.com/priytamjeepandey/forge-ci/internal/vcs/vcstest"
)

// TestNativeChangeAPI drives a Forge-native change on a plain git server
// through the API: open, edit, the merge queue's endpoints, and a merge the
// runner does over the task protocol.
func TestNativeChangeAPI(t *testing.T) {
	if !mirror.MergeSupported() {
		t.Skip("git 2.38+ not installed")
	}
	t.Setenv("ACQUIRE_LONG_POLL", "300ms")
	g := vcstest.NewFakeGit(t)
	st, db := newCFRStore(t, "")
	h := &apiHarness{t: t, st: st, db: db, srv: New(st, blob.NewLocal(t.TempDir()), nil)}
	ctx := context.Background()
	repo := g.Repo()
	if err := st.RegisterRepo(ctx, proto.RepoRegistration{Repo: repo.Repo, Provider: "git", CloneURL: repo.CloneURL,
		DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	head := g.CommitFile(t, "feature", "f.txt", "feature\n")
	open := testCaller{}
	newChange := func(m map[string]any) *httptest.ResponseRecorder {
		body := map[string]any{"repo": repo.Repo, "source_branch": "feature", "target_branch": "main", "title": "Seat lock",
			"author": "dev@acme.io"}
		for k, v := range m {
			body[k] = v
		}
		return h.do(open, http.MethodPost, "/api/v1/changes", body)
	}
	h.expect(newChange(map[string]any{"title": " "}), http.StatusBadRequest, "no title")
	h.expect(newChange(map[string]any{"target_branch": "feature"}), http.StatusBadRequest, "same branches")
	h.expect(newChange(map[string]any{"source_branch": "--upload-pack=x"}), http.StatusBadRequest, "bad branch")
	h.expect(newChange(map[string]any{"source_branch": "forge-queue/main"}), http.StatusBadRequest, "queue branch")
	h.expect(newChange(map[string]any{"repo": "acme/nope"}), http.StatusNotFound, "unknown repo")
	rec := newChange(nil)
	h.expect(rec, http.StatusCreated, "create")
	var created struct {
		ID     int64  `json:"id"`
		Number string `json:"number"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	h.expect(newChange(nil), http.StatusConflict, "duplicate")
	svc := review.New(st)
	svc.RefreshDue(ctx)
	path := "/api/v1/changes/" + strconv.FormatInt(created.ID, 10)

	type page struct {
		Change       store.ChangeDetail `json:"change"`
		Capabilities struct {
			MergeMethods []string `json:"merge_methods"`
		} `json:"capabilities"`
		Queue       struct{ Enabled bool } `json:"queue"`
		Permissions map[string]bool        `json:"permissions"`
	}
	get := func() page {
		var p page
		_ = json.Unmarshal(h.do(open, http.MethodGet, path, nil).Body.Bytes(), &p)
		return p
	}
	p := get()
	if p.Change.HeadSHA != head || p.Change.Number != "1" || !p.Permissions["can_edit"] ||
		strings.Join(p.Capabilities.MergeMethods, ",") != "merge,squash,fast_forward" {
		t.Fatalf("native change page = %+v", p)
	}
	h.expect(h.do(open, http.MethodPatch, path, map[string]any{"state": "merged"}), http.StatusBadRequest, "bad state")
	h.expect(h.do(open, http.MethodPatch, path, map[string]any{"title": "Seat-level lock", "body": "Why: …"}), http.StatusNoContent, "edit")
	if p := get(); p.Change.Title != "Seat-level lock" || p.Change.Body != "Why: …" {
		t.Fatalf("after edit = %q %q", p.Change.Title, p.Change.Body)
	}

	// With the queue on, Merge is refused and the queue takes the change.
	h.expect(h.do(open, http.MethodPut, "/api/v1/merge-policies", map[string]any{"repo": repo.Repo, "branch_pattern": "main",
		"required_approvals": 0, "require_ci": false, "merge_queue": true}), http.StatusOK, "rule with queue")
	svc.EvaluateGates(ctx)
	if !get().Queue.Enabled {
		t.Fatal("the page does not say the queue is on")
	}
	h.expect(h.do(open, http.MethodPost, path+"/merge", map[string]any{"expected_head": head}), http.StatusConflict, "merge with the queue on")
	h.expect(h.do(open, http.MethodPost, path+"/queue", map[string]any{"expected_head": "stale"}), http.StatusConflict, "stale head")
	h.expect(h.do(open, http.MethodPost, path+"/queue", map[string]any{"expected_head": head}), http.StatusAccepted, "queue")
	h.expect(h.do(open, http.MethodPost, path+"/queue", map[string]any{"expected_head": head}), http.StatusConflict, "queue twice")
	var list []store.QueueEntry
	_ = json.Unmarshal(h.do(open, http.MethodGet, "/api/v1/merge-queue?repo="+repo.Repo+"&target=main", nil).Body.Bytes(), &list)
	if len(list) != 1 || list[0].Number != "1" || list[0].Position != 1 {
		t.Fatalf("queue = %+v", list)
	}
	h.expect(h.do(open, http.MethodDelete, path+"/queue", nil), http.StatusNoContent, "dequeue")
	h.expect(h.do(open, http.MethodDelete, path+"/queue", nil), http.StatusNotFound, "dequeue twice")

	// Without the queue: Merge goes to a runner over the task protocol.
	h.expect(h.do(open, http.MethodPut, "/api/v1/merge-policies", map[string]any{"repo": repo.Repo, "branch_pattern": "main",
		"required_approvals": 0, "require_ci": false}), http.StatusOK, "rule without queue")
	svc.EvaluateGates(ctx)
	h.expect(h.do(open, http.MethodPost, path+"/merge", map[string]any{"expected_head": head, "method": "merge"}),
		http.StatusAccepted, "merge")
	svc.SendOutbox(ctx) // queues the runner task
	rec = h.do(open, http.MethodPost, "/api/v1/runner/tasks/acquire",
		proto.TaskAcquireRequest{RunnerID: "r1", Kinds: []string{proto.TaskKindMerge}})
	h.expect(rec, http.StatusOK, "acquire merge")
	var task proto.RunnerTask
	if err := json.Unmarshal(rec.Body.Bytes(), &task); err != nil || task.Merge == nil || task.Merge.ExpectedSource != head {
		t.Fatalf("merge task = %s", rec.Body.String())
	}
	res, err := mirror.Open(filepath.Join(t.TempDir(), "r1"), task.Repo).Merge(ctx, task.CloneURL, task.RedactValues, *task.Merge)
	if err != nil {
		t.Fatal(err)
	}
	h.expect(h.do(open, http.MethodPost, fmt.Sprintf("/api/v1/runner/tasks/%d/complete", task.ID),
		proto.TaskCompleteRequest{RunnerID: "r1", Merge: res}), http.StatusNoContent, "complete merge")
	if _, err := db.Exec(ctx, `UPDATE provider_outbox SET next_attempt_at = now()`); err != nil {
		t.Fatal(err)
	}
	svc.SendOutbox(ctx)
	if p := get(); p.Change.State != "merged" || g.Tip(t, "main") != res.SHA {
		t.Fatalf("after the runner merged: state %s, main %s, merge %s", p.Change.State, g.Tip(t, "main"), res.SHA)
	}
	h.expect(h.do(open, http.MethodPatch, path, map[string]any{"title": "x"}), http.StatusConflict, "edit merged")
}

func TestQueueBranchPushesStartNoPipeline(t *testing.T) {
	st, db := newCFRStore(t, "")
	h := &apiHarness{t: t, st: st, db: db, srv: New(st, blob.NewLocal(t.TempDir()), nil)}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/github", nil)
	if h.srv.triggerFromWebhook(rec, req, webhookRun{Repo: "acme/shop", Ref: "refs/heads/forge-queue/main",
		SHA: strings.Repeat("a", 40), Source: "push"}) || rec.Code != http.StatusAccepted {
		t.Fatalf("queue branch push = %d %s", rec.Code, rec.Body.String())
	}
	var n int
	_ = db.QueryRow(context.Background(), `SELECT count(*) FROM pipelines`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d pipelines started from a queue branch push", n)
	}
}
