package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/blob"
	"github.com/priytamjeepandey/forge-ci/internal/mirror"
	"github.com/priytamjeepandey/forge-ci/internal/proto"
	"github.com/priytamjeepandey/forge-ci/internal/review"
	"github.com/priytamjeepandey/forge-ci/internal/store"
	"github.com/priytamjeepandey/forge-ci/internal/vcs"
)

// gitHost is a git server on disk laid out like GitHub: branches plus
// refs/pull/<n>/head, serving partial clones.
type gitHost struct {
	t          *testing.T
	bare, work string
}

func newGitHost(t *testing.T) *gitHost {
	t.Helper()
	if !mirror.Supported() {
		t.Skip("git 2.27+ not installed")
	}
	dir := t.TempDir()
	g := &gitHost{t: t, bare: filepath.Join(dir, "origin.git"), work: filepath.Join(dir, "work")}
	g.run(dir, "init", "-q", "--bare", "-b", "main", g.bare)
	g.run(g.bare, "config", "uploadpack.allowFilter", "true")
	g.run(dir, "clone", "-q", g.bare, g.work)
	return g
}

func (g *gitHost) run(dir string, args ...string) string {
	g.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t",
		"GIT_COMMITTER_EMAIL=t@x", "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		g.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (g *gitHost) commit(path, content string) string {
	full := filepath.Join(g.work, path)
	_ = os.MkdirAll(filepath.Dir(full), 0o755)
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		g.t.Fatal(err)
	}
	g.run(g.work, "add", "-A")
	g.run(g.work, "commit", "-q", "-m", "edit "+path)
	return g.run(g.work, "rev-parse", "HEAD")
}

type diffHarness struct {
	*apiHarness
	host  *gitHost
	root  string // the fake runner's mirror dir
	repo  string
	id    int64
	base  string
	heads []string
}

// newDiffHarness: a GitHub-registered repo whose clone URL is the on-disk
// host, and one open pull request (#1) with one revision.
func newDiffHarness(t *testing.T) *diffHarness {
	t.Helper()
	t.Setenv("ACQUIRE_LONG_POLL", "300ms")
	t.Setenv("MIRROR_AFFINITY_WAIT", "1h")
	g := newGitHost(t)
	st, db := newCFRStore(t, "")
	h := &diffHarness{apiHarness: &apiHarness{t: t, st: st, db: db, srv: New(st, blob.NewLocal(t.TempDir()), nil)},
		host: g, root: t.TempDir(), repo: "acme/shop"}
	if err := st.RegisterRepo(context.Background(), proto.RepoRegistration{Repo: h.repo, Provider: "github",
		CloneURL: "file://" + g.bare, DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	g.commit("README.md", "hello\n")
	h.base = g.commit("app.go", "package app\n")
	g.run(g.work, "push", "-q", "origin", "main")
	g.run(g.work, "checkout", "-q", "-b", "feature")
	h.push("app.go", "package app\n\nfunc New() {}\n")
	return h
}

// push adds a commit to the pull request and tells Forge (as a webhook would).
func (h *diffHarness) push(path, content string) string {
	h.t.Helper()
	head := h.host.commit(path, content)
	h.host.run(h.host.work, "push", "-q", "-f", "origin", "feature", "feature:refs/pull/1/head")
	h.heads = append(h.heads, head)
	id, _, err := h.st.ApplyChange(context.Background(), h.repo, "github", &vcs.Change{ID: "1", Title: "New()",
		Author: "dev", SourceBranch: "feature", TargetBranch: "main", State: "open", HeadSHA: head, BaseSHA: h.base,
		UpdatedAt: time.Now().Add(time.Duration(len(h.heads)) * time.Second)})
	if err != nil {
		h.t.Fatal(err)
	}
	h.id = id
	return head
}

// runnerDo acquires one task as runner and runs it the way forge-runner
// does; it reports false when no task was handed out.
func (h *diffHarness) runnerDo(runner string) bool {
	h.t.Helper()
	rec := h.do(testCaller{}, http.MethodPost, "/api/v1/runner/tasks/acquire",
		proto.TaskAcquireRequest{RunnerID: runner, Kinds: []string{proto.TaskKindDiff}})
	if rec.Code == http.StatusNoContent {
		return false
	}
	h.expect(rec, http.StatusOK, "task acquire")
	var task proto.RunnerTask
	if err := json.Unmarshal(rec.Body.Bytes(), &task); err != nil || task.Diff == nil || task.CloneURL == "" {
		h.t.Fatalf("task = %s", rec.Body.String())
	}
	report := proto.TaskCompleteRequest{RunnerID: runner}
	res, err := mirror.Open(filepath.Join(h.root, runner), task.Repo).Diff(context.Background(), task.CloneURL, task.RedactValues, *task.Diff)
	if err != nil {
		report.Error = err.Error()
	} else {
		report.Diff = res
	}
	h.expect(h.do(testCaller{}, http.MethodPost, fmt.Sprintf("/api/v1/runner/tasks/%d/complete", task.ID), report),
		http.StatusNoContent, "task complete")
	return true
}

func (h *diffHarness) diff(query string) diffView {
	h.t.Helper()
	rec := h.do(testCaller{}, http.MethodGet, "/api/v1/changes/"+strconv.FormatInt(h.id, 10)+"/diff"+query, nil)
	h.expect(rec, http.StatusOK, "diff "+query)
	var v diffView
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	return v
}

// TestDiffEndToEnd: the diff is queued by the change write, computed by a
// runner from its mirror, stored once, and served immutably — with no
// provider API call anywhere.
func TestDiffEndToEnd(t *testing.T) {
	h := newDiffHarness(t)
	if v := h.diff(""); v.Status != "pending" {
		t.Fatalf("before any runner: %+v", v)
	}
	if !h.runnerDo("runner-a") {
		t.Fatal("no task for the change's diff")
	}
	v := h.diff("")
	if v.Status != "ready" || v.Files != 1 || v.Additions != 2 || v.MergeBase != h.base || v.ManifestURL == "" {
		t.Fatalf("ready diff = %+v", v)
	}
	rec := h.do(testCaller{}, http.MethodGet, v.ManifestURL, nil)
	h.expect(rec, http.StatusOK, "manifest")
	if cc := rec.Header().Get("Cache-Control"); cc != immutableCache {
		t.Errorf("manifest Cache-Control = %q", cc)
	}
	var m review.DiffManifest
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil || len(m.Files) != 1 || m.Files[0].Path != "app.go" ||
		!strings.Contains(m.Files[0].Patch, "+func New() {}") {
		t.Fatalf("manifest = %s", rec.Body.String())
	}
	req := httptest.NewRequest(http.MethodGet, v.ManifestURL, nil)
	req.Header.Set("If-None-Match", rec.Header().Get("ETag"))
	rec2 := httptest.NewRecorder()
	h.srv.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusNotModified {
		t.Errorf("revalidated manifest = %d", rec2.Code)
	}
	for _, bad := range []string{"../../etc/passwd", "zz/change/a..b/manifest.json", strings.Repeat("a", 16) + "/change/x..y/manifest.json"} {
		// 404, or the router's redirect to the cleaned path (which 404s).
		if rec := h.do(testCaller{}, http.MethodGet, "/api/v1/diff-blobs/"+bad, nil); rec.Code == http.StatusOK {
			t.Errorf("blob path %q served: %s", bad, rec.Body.String())
		}
	}

	// A second push: the new head's diff is queued; the repo's mirror lives on
	// runner-a, so runner-b is not handed it while runner-a is online.
	if _, err := h.db.Exec(context.Background(),
		`INSERT INTO runners (id, last_contact_at) VALUES ('runner-a', now()) ON CONFLICT (id) DO UPDATE SET last_contact_at = now()`); err != nil {
		t.Fatal(err)
	}
	h.push("app.go", "package app\n\nfunc New() {}\n\nfunc Old() {}\n")
	if h.runnerDo("runner-b") {
		t.Fatal("runner-b took a diff whose mirror lives on runner-a")
	}
	if !h.runnerDo("runner-a") {
		t.Fatal("runner-a was not handed its repo's diff")
	}
	if v := h.diff(""); v.Status != "ready" || v.Additions != 4 {
		t.Errorf("second revision diff = %+v", v)
	}

	// Changes since revision 1: computed on demand, then served.
	q := "?from=" + h.heads[0] + "&to=" + h.heads[1]
	if v := h.diff(q); v.Status != "pending" || v.Mode != "range" {
		t.Fatalf("range before compute = %+v", v)
	}
	if !h.runnerDo("runner-a") {
		t.Fatal("no task for the range diff")
	}
	if v := h.diff(q); v.Status != "ready" || v.Files != 1 || v.Additions != 2 {
		t.Errorf("range = %+v", v)
	}
	h.expect(h.do(testCaller{}, http.MethodGet, "/api/v1/changes/"+strconv.FormatInt(h.id, 10)+"/diff?from="+h.base+"&to="+h.heads[1], nil),
		http.StatusBadRequest, "range from a commit that is not a revision")

	var stats codeReviewStats
	_ = json.Unmarshal(h.do(testCaller{}, http.MethodGet, "/api/v1/code-review/metrics", nil).Body.Bytes(), &stats)
	if stats.TotalCalls != 0 || stats.ReadPathCalls != 0 {
		t.Errorf("provider API calls = %d (read path %d): diffs must never use a provider API", stats.TotalCalls, stats.ReadPathCalls)
	}
	if stats.Diffs.Computed != 3 || stats.Diffs.Reads["hit"] < 3 || stats.Diffs.Reads["miss"] != 1 || stats.DiffOpenSamples == 0 {
		t.Errorf("diff stats = %+v (opens %d)", stats.Diffs, stats.DiffOpenSamples)
	}
}

// TestDiffTaskFailureAndRetry: a diff that cannot be computed fails after
// its retries with the reason on the page, and opening it again re-queues it.
func TestDiffTaskFailureAndRetry(t *testing.T) {
	h := newDiffHarness(t)
	// The pull request's commits vanish from the host (force-push away).
	h.host.run(h.host.bare, "update-ref", "-d", "refs/pull/1/head")
	h.host.run(h.host.bare, "update-ref", "-d", "refs/heads/feature")
	h.host.run(h.host.bare, "gc", "-q", "--prune=now")
	for i := 0; i < 3; i++ {
		if _, err := h.db.Exec(context.Background(), `UPDATE runner_tasks SET not_before = now()`); err != nil {
			t.Fatal(err)
		}
		h.runnerDo("runner-a")
	}
	v := h.diff("")
	if v.Status != "failed" || !strings.Contains(v.Error, "not on the host") {
		t.Fatalf("failed diff = %+v", v)
	}
	if v := h.diff("?retry=1"); v.Status != "pending" {
		t.Errorf("retry = %+v", v)
	}
}

// TestDiffManifestLayout: small patches inline, big and generated ones as
// chunks fetched on expand.
func TestDiffManifestLayout(t *testing.T) {
	k := store.DiffKey{Repo: "acme/shop", From: strings.Repeat("a", 40), To: strings.Repeat("b", 40), Mode: "change"}
	res := &proto.DiffResult{MergeBase: strings.Repeat("c", 40), Files: []proto.DiffFile{
		{Path: "a.go", Additions: 1, Patch: "diff --git a/a.go b/a.go\n+x\n"},
		{Path: "go.sum", Generated: true, Patch: "diff --git a/go.sum b/go.sum\n+y\n"},
		{Path: "big.go", Patch: "diff --git a/big.go b/big.go\n" + strings.Repeat("+z\n", 20000)},
		{Path: "logo.png", Binary: true},
	}}
	m, chunks := review.BuildManifest(k, res)
	if m.Files[0].Patch == "" || m.Files[1].Chunk != "f/1.patch" || m.Files[2].Chunk != "f/2.patch" ||
		m.Files[3].Chunk != "" || len(chunks) != 2 || m.Additions != 1 {
		t.Errorf("manifest = %+v chunks %d", m.Files, len(chunks))
	}
	if p := review.BlobPrefix(k, res.MergeBase); !strings.HasSuffix(p, "/change/"+res.MergeBase+".."+k.To) {
		t.Errorf("prefix = %s", p)
	}
	if _, ok := review.BlobKeyFromURL(review.RepoKey("x"), "change", k.From+".."+k.To, "f/1.patch"); !ok {
		t.Error("valid chunk rejected")
	}
	if _, ok := review.BlobKeyFromURL(review.RepoKey("x"), "change", k.From+".."+k.To, "f/../../x"); ok {
		t.Error("traversal accepted")
	}
	var buf bytes.Buffer
	_ = json.NewEncoder(&buf).Encode(m)
	if buf.Len() > 400<<10 {
		t.Errorf("manifest is %d bytes: big files must not be inline", buf.Len())
	}
}

// TestDiffOpensMeetTheTargets is phase C's "done when": once warm, opening a
// diff (status + manifest) stays under 300 ms p95 and over 99% of opens are
// served ready.
func TestDiffOpensMeetTheTargets(t *testing.T) {
	h := newDiffHarness(t)
	h.diff("") // the one miss: queued by the change write, opened before the runner ran
	h.runnerDo("runner-a")
	for i := 0; i < 200; i++ {
		v := h.diff("")
		h.expect(h.do(testCaller{}, http.MethodGet, v.ManifestURL, nil), http.StatusOK, "manifest")
	}
	var st codeReviewStats
	_ = json.Unmarshal(h.do(testCaller{}, http.MethodGet, "/api/v1/code-review/metrics", nil).Body.Bytes(), &st)
	if st.DiffOpenSamples < 400 || st.DiffOpenP95MS >= 300 {
		t.Errorf("diff open p95 = %.1f ms over %d opens, want < 300", st.DiffOpenP95MS, st.DiffOpenSamples)
	}
	if st.Diffs.HitRatio <= 0.99 {
		t.Errorf("hit ratio = %.4f (%+v), want > 0.99", st.Diffs.HitRatio, st.Diffs.Reads)
	}
	t.Logf("diff open p95 %.2f ms over %d opens; hit ratio %.4f", st.DiffOpenP95MS, st.DiffOpenSamples, st.Diffs.HitRatio)
}

// TestTaskAcquirePausedHoldsThePoll: a paused runner gets no task, but only
// after the full long poll — an instant 204 made it ask again at once.
func TestTaskAcquirePausedHoldsThePoll(t *testing.T) {
	h := newDiffHarness(t)
	ctx := context.Background()
	if _, err := h.st.TouchRunner(ctx, "paused-1", "shell", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.SetRunnerPaused(ctx, "paused-1", true); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if h.runnerDo("paused-1") {
		t.Fatal("a paused runner was handed a task")
	}
	if took := time.Since(start); took < 250*time.Millisecond {
		t.Fatalf("paused acquire answered in %v; it must hold the 300ms poll", took)
	}
}
