package api

import (
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
	"github.com/priytamjeepandey/forge-ci/internal/review"
	"github.com/priytamjeepandey/forge-ci/internal/store"
	"github.com/priytamjeepandey/forge-ci/internal/vcs"
)

// Diffs (code review phase C). Runners compute them from their git mirrors
// through the task protocol below; pages read them through two read routes:
// the diff's status (tiny, ETag'd) and its immutable blobs (cached by the
// browser forever, and by this process for repeat opens). Neither touches a
// provider.

const (
	taskLease          = 5 * time.Minute
	maxTaskReportBytes = 64 << 20
	diffCacheBytes     = 32 << 20
	// immutableCache: the URL names commits, so its bytes never change.
	// private: diffs of private repos must never sit in a shared cache.
	immutableCache = "private, max-age=31536000, immutable"
)

// mirrorAffinityWait is how long a diff waits for the runner that holds the
// repo's mirror before any runner takes it (MIRROR_AFFINITY_WAIT).
func mirrorAffinityWait() time.Duration {
	if d, err := time.ParseDuration(os.Getenv("MIRROR_AFFINITY_WAIT")); err == nil && d >= 0 {
		return d
	}
	return 10 * time.Second
}

func (s *Server) registerRunnerTaskRoutes() {
	s.mux.HandleFunc("POST /api/v1/runner/tasks/acquire", s.acquireTask)
	s.mux.HandleFunc("POST /api/v1/runner/tasks/{id}/complete", s.completeTask)
}

// acquireTask long-polls for a runner task, like job acquire. The clone
// credential is minted now, so it is fresh for the whole task.
func (s *Server) acquireTask(w http.ResponseWriter, r *http.Request) {
	if !s.requireRunnerAuth(w, r) {
		return
	}
	var req proto.TaskAcquireRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.RunnerID == "" || len(req.Kinds) == 0 {
		writeErr(w, http.StatusBadRequest, "runner_id and kinds are required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), acquireLongPoll())
	defer cancel()
	for ctx.Err() == nil {
		// A paused runner still holds the poll for its full length: answering
		// at once makes the runner ask again at once, a hot loop on the DB.
		if s.taskPaused(ctx, req.RunnerID) {
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
			continue
		}
		t, err := s.store.ClaimRunnerTask(ctx, req.RunnerID, req.Kinds, mirrorAffinityWait(), taskLease)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			slog.Error("runner task: claim", "err", err)
			writeErr(w, http.StatusInternalServerError, "queue error")
			return
		}
		if t != nil {
			if task, ok := s.prepareTask(ctx, t); ok {
				writeJSON(w, http.StatusOK, task)
				return
			}
			continue
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// taskPaused: a paused or draining runner, or a server in maintenance, takes
// no tasks either.
func (s *Server) taskPaused(ctx context.Context, runnerID string) bool {
	if mm, err := s.store.Maintenance(ctx); err == nil && mm.On {
		return true
	}
	st, err := s.store.RunnerState(ctx, runnerID)
	return err == nil && (st.Paused || st.Draining)
}

// prepareTask turns a claimed row into what the runner needs; a task that
// can never run (repo disconnected) is failed for good.
func (s *Server) prepareTask(ctx context.Context, t *store.TaskRow) (*proto.RunnerTask, bool) {
	fail := func(reason string) (*proto.RunnerTask, bool) {
		_ = s.store.FailRunnerTask(ctx, t.ID, reason)
		if t.Kind == proto.TaskKindDiff {
			if k, _, err := store.DiffKeyFromTask(t); err == nil {
				_ = s.store.MarkDiffFailed(ctx, k, reason)
			}
		}
		return nil, false
	}
	task := &proto.RunnerTask{ID: t.ID, Kind: t.Kind, Repo: t.Repo}
	switch t.Kind {
	case proto.TaskKindDiff:
		_, d, err := store.DiffKeyFromTask(t)
		if err != nil {
			return fail(err.Error())
		}
		task.Diff = &d
	case proto.TaskKindMerge:
		m, err := store.MergeTaskFromRow(t)
		if err != nil {
			return fail(err.Error())
		}
		task.Merge = &m
	default:
		return fail("unknown task kind " + t.Kind)
	}
	cloneURL, redact, ok, err := s.store.TaskCloneURL(ctx, t.Repo)
	if err != nil {
		slog.Error("runner task: clone credential", "repo", t.Repo, "err", err)
		return fail("could not get a clone credential for " + t.Repo)
	}
	if !ok {
		return fail(t.Repo + " is not connected to a host git can fetch from")
	}
	task.CloneURL, task.RedactValues = cloneURL, redact
	return task, true
}

// completeTask records a task's outcome; a diff's bytes go to the blob store.
func (s *Server) completeTask(w http.ResponseWriter, r *http.Request) {
	if !s.requireRunnerAuth(w, r) {
		return
	}
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid task id")
		return
	}
	var req proto.TaskCompleteRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxTaskReportBytes)).Decode(&req); err != nil || req.RunnerID == "" {
		writeErr(w, http.StatusBadRequest, "invalid task report")
		return
	}
	failure := req.Error
	if failure == "" && req.Diff == nil && req.Merge == nil {
		failure = "the runner reported no result"
	}
	if failure == "" && req.Merge != nil {
		// A merge's answer is kept on the task; whoever queued it reads it.
		if err := s.store.SetRunnerTaskResult(r.Context(), id, req.RunnerID, req.Merge); errors.Is(err, store.ErrTaskGone) {
			writeErr(w, http.StatusConflict, err.Error())
			return
		} else if err != nil {
			writeErr(w, http.StatusInternalServerError, "failed to record the merge")
			return
		}
	} else if failure == "" {
		// Store before finishing, so a task is only done once its diff is.
		t, err := s.store.ClaimedTask(r.Context(), id, req.RunnerID)
		if errors.Is(err, store.ErrTaskGone) {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "failed to load task")
			return
		}
		k, _, err := store.DiffKeyFromTask(t)
		if err == nil {
			err = review.StoreDiff(r.Context(), s.store, s.blobs, k, req.Diff)
		}
		if err != nil {
			slog.Error("runner task: store diff", "task", id, "err", err)
			failure = "storing the diff failed: " + err.Error()
		}
	}
	t, final, err := s.store.FinishRunnerTask(r.Context(), id, req.RunnerID, failure)
	if errors.Is(err, store.ErrTaskGone) {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to record task")
		return
	}
	if failure != "" && final && t.Kind == proto.TaskKindDiff {
		if k, _, err := store.DiffKeyFromTask(t); err == nil {
			_ = s.store.MarkDiffFailed(r.Context(), k, failure)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- reading diffs ----

// diffView is the status of a diff, and where its manifest is when ready.
type diffView struct {
	Status      string `json:"status"` // ready | pending | failed | unsupported
	Mode        string `json:"mode"`
	From        string `json:"from"`
	To          string `json:"to"`
	MergeBase   string `json:"merge_base,omitempty"`
	Files       int    `json:"files"`
	Additions   int    `json:"additions"`
	Deletions   int    `json:"deletions"`
	Error       string `json:"error,omitempty"`
	ManifestURL string `json:"manifest_url,omitempty"`
}

// getChangeDiff: GET /api/v1/changes/{id}/diff — the change's diff against
// its target (default), or ?from=&to= between two of its revisions. A diff
// not computed yet is queued for a runner (a database write, never a
// provider call) and the page is told when it is ready over the change
// stream.
func (s *Server) getChangeDiff(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	defer func() { s.diffOpens.add(time.Since(start)) }()
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid change id")
		return
	}
	ctx := r.Context()
	d, err := s.store.GetChangeDetail(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "change not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to load change")
		return
	}
	k := store.DiffKey{Repo: d.Repo, From: d.BaseSHA, To: d.HeadSHA, Mode: "change"}
	if from, to := r.URL.Query().Get("from"), r.URL.Query().Get("to"); from != "" || to != "" {
		known := map[string]bool{}
		for _, rev := range d.RevisionList {
			known[rev.HeadSHA] = true
		}
		if !known[from] || !known[to] || from == to {
			writeErr(w, http.StatusBadRequest, "from and to must be two different revisions of this change")
			return
		}
		k = store.DiffKey{Repo: d.Repo, From: from, To: to, Mode: "range"}
	}
	view := diffView{Mode: k.Mode, From: k.From, To: k.To}
	refs, mirrorable := vcs.MirrorRefsFor(d.Provider, d.Number, d.SourceBranch, d.TargetBranch)
	if !mirrorable || k.From == "" || k.To == "" {
		view.Status = "unsupported"
		writeJSONCached(w, r, view)
		return
	}
	row, err := s.store.GetDiff(ctx, k)
	result := ""
	switch {
	case errors.Is(err, store.ErrNotFound) || (err == nil && row.State == "failed" && r.URL.Query().Get("retry") == "1"):
		row, err = s.store.RequestDiff(ctx, k, refs, true)
		result = "miss"
	case err == nil && row.State == "ready":
		result = "hit"
		_ = s.store.TouchDiffRead(ctx, k)
	case err == nil:
		result = row.State
	}
	if err != nil {
		slog.Error("get diff", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to load diff")
		return
	}
	if rerr := s.store.RecordDiffRead(ctx, result); rerr != nil {
		slog.Warn("record diff read", "err", rerr)
	}
	view.Status, view.Error, view.MergeBase = row.State, row.Error, row.MergeBase
	view.Files, view.Additions, view.Deletions = row.Files, row.Additions, row.Deletions
	if row.State == "ready" {
		view.ManifestURL = "/api/v1/diff-blobs/" + strings.TrimPrefix(row.BlobPrefix, "diffs/v1/") + "/manifest.json"
	}
	writeJSONCached(w, r, view)
}

// getDiffBlob serves a diff manifest or chunk. The URL names commits, so the
// response is immutable: the browser keeps it forever and never asks again.
func (s *Server) getDiffBlob(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	key, ok := review.BlobKeyFromURL(r.PathValue("repo"), r.PathValue("mode"), r.PathValue("range"), r.PathValue("rest"))
	if !ok {
		writeErr(w, http.StatusNotFound, "no such diff")
		return
	}
	etag := `"` + review.RepoKey(key) + `"`
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	body, ok := s.diffCache.get(key)
	if !ok {
		if s.blobs == nil {
			writeErr(w, http.StatusServiceUnavailable, "no blob store configured")
			return
		}
		rc, err := s.blobs.Get(r.Context(), key)
		if err != nil {
			writeErr(w, http.StatusNotFound, "diff not stored (it may have expired; reopen the change to recompute)")
			return
		}
		body, err = io.ReadAll(io.LimitReader(rc, maxTaskReportBytes))
		rc.Close()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "failed to read diff")
			return
		}
		s.diffCache.put(key, body)
	}
	ct := "text/plain; charset=utf-8"
	if strings.HasSuffix(key, ".json") {
		ct = "application/json"
		defer func() { s.diffOpens.add(time.Since(start)) }()
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", immutableCache)
	w.Header().Set("ETag", etag)
	_, _ = w.Write(body)
}

// ---- in-process caches and timings ----

// byteLRU keeps recently served diff blobs in memory, bounded by bytes.
type byteLRU struct {
	mu    sync.Mutex
	max   int
	size  int
	order *list.List
	items map[string]*list.Element
}

type lruEntry struct {
	key  string
	body []byte
}

func newByteLRU(max int) *byteLRU {
	return &byteLRU{max: max, order: list.New(), items: map[string]*list.Element{}}
}

func (c *byteLRU) get(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok {
		c.order.MoveToFront(e)
		return e.Value.(*lruEntry).body, true
	}
	return nil, false
}

func (c *byteLRU) put(key string, body []byte) {
	if len(body) > c.max/4 {
		return // one huge chunk must not evict everything
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.items[key]; ok {
		return
	}
	c.items[key] = c.order.PushFront(&lruEntry{key: key, body: body})
	c.size += len(body)
	for c.size > c.max {
		last := c.order.Back()
		e := last.Value.(*lruEntry)
		c.order.Remove(last)
		delete(c.items, e.key)
		c.size -= len(e.body)
	}
}

// latencyRing keeps the last durations of diff opens on this instance.
type latencyRing struct {
	mu   sync.Mutex
	vals []time.Duration
	next int
	full bool
}

func newLatencyRing(n int) *latencyRing { return &latencyRing{vals: make([]time.Duration, n)} }

func (l *latencyRing) add(d time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.vals[l.next] = d
	l.next = (l.next + 1) % len(l.vals)
	l.full = l.full || l.next == 0
}

// p95 returns the 95th percentile and how many samples it is over.
func (l *latencyRing) p95() (time.Duration, int) {
	l.mu.Lock()
	n := l.next
	if l.full {
		n = len(l.vals)
	}
	s := append([]time.Duration(nil), l.vals[:n]...)
	l.mu.Unlock()
	if n == 0 {
		return 0, 0
	}
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[(n*95+99)/100-1], n
}
