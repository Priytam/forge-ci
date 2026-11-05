package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/review"
	"github.com/priytamjeepandey/forge-ci/internal/store"
	"github.com/priytamjeepandey/forge-ci/internal/vcs"
)

// Code review (batch 7). Every route here that VIEWS changes is a read route:
// it is registered through changeReadRoutes, served with a read-path context
// (any provider call made with it is refused and counted), and reads
// Postgres only. Webhooks write; pages read.

// changeReadRoutes are all pages/APIs used to view changes. The read-path
// test walks this table, so a new view route is covered by adding it here.
func (s *Server) changeReadRoutes() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"GET /api/v1/changes":                                    s.listChanges,
		"GET /api/v1/changes/{id}":                               s.getChange,
		"GET /api/v1/changes/stream":                             s.streamChanges,
		"GET /api/v1/code-review/metrics":                        s.codeReviewMetrics,
		"GET /api/v1/code-review/capabilities":                   s.providerCapabilities,
		"GET /api/v1/merge-policies":                             s.listMergePolicies,
		"GET /api/v1/host-identities":                            s.listHostIdentities,
		"GET /api/v1/changes/{id}/diff":                          s.getChangeDiff,
		"GET /api/v1/diff-blobs/{repo}/{mode}/{range}/{rest...}": s.getDiffBlob,
		"GET /api/v1/merge-queue":                                s.listMergeQueue,
	}
}

func (s *Server) registerCodeReviewRoutes() {
	for pattern, h := range s.changeReadRoutes() {
		s.mux.HandleFunc(pattern, readPath(h))
	}
	s.registerCodeReviewWriteRoutes()
	s.registerThreadRoutes()
	s.registerNativeChangeRoutes()
}

// readPath serves h with a read-path context.
func readPath(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h(w, r.WithContext(vcs.ReadPath(r.Context())))
	}
}

// ingestChanges records a webhook delivery's change events. It never fails
// the delivery: CI triggering must not depend on code review.
func (s *Server) ingestChanges(r *http.Request, provider string, body []byte) {
	n, err := review.Ingest(r.Context(), s.store, provider, r.Header, body)
	if err != nil {
		slog.Error("code review: ingest webhook", "provider", provider, "err", err)
		return
	}
	if n > 0 {
		slog.Debug("code review: webhook ingested", "provider", provider, "events", n)
	}
}

// writeJSONCached writes v with a strong ETag and answers 304 when the
// client already has it, so a page refetching after a live event costs no
// body when its object did not change.
func writeJSONCached(w http.ResponseWriter, r *http.Request, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "encode failed")
		return
	}
	sum := sha256.Sum256(b)
	etag := `"` + hex.EncodeToString(sum[:12]) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, no-cache")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(append(b, '\n'))
}

// listChanges is the inbox: GET /api/v1/changes?state=open|merged|closed|all
// &repo=&author=&q=&limit=&offset=. X-Total-Count carries the match count.
func (s *Server) listChanges(w http.ResponseWriter, r *http.Request) {
	qs := r.URL.Query()
	limit, offset := pageParams(r)
	q := store.ChangeQuery{
		State: qs.Get("state"), Repo: qs.Get("repo"), Author: qs.Get("author"),
		Search: strings.TrimSpace(qs.Get("q")), Limit: limit, Offset: offset,
	}
	changes, total, err := s.store.ListChanges(r.Context(), q)
	if err != nil {
		if strings.HasPrefix(err.Error(), "state must") {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		slog.Error("list changes", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to list changes")
		return
	}
	w.Header().Set("X-Total-Count", strconv.Itoa(total))
	w.Header().Set("X-Has-More", strconv.FormatBool(offset+len(changes) < total))
	writeJSONCached(w, r, changes)
}

// getChange is the change page: the change, its revisions and reviews, and
// what its host supports natively (the UI branches on that, never on the
// provider's name).
func (s *Server) getChange(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid change id")
		return
	}
	d, err := s.store.GetChangeDetail(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "change not found")
		return
	}
	if err != nil {
		slog.Error("get change", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to load change")
		return
	}
	ctx := r.Context()
	gate, err := s.store.GetGate(ctx, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to load the merge gate")
		return
	}
	actions, err := s.store.ChangeActions(ctx, id, 10)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to load actions")
		return
	}
	// What the caller may do here — a hint for the UI; every write re-checks.
	c := &store.ReviewTargetChange{ID: d.ID, Repo: d.Repo, Provider: d.Provider, Author: d.Author, TargetBranch: d.TargetBranch}
	canReview := false
	policy, err := s.changePolicy(ctx, c)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to load merge rules")
		return
	}
	if who := s.reviewerFor(r, ""); who != "" {
		canReview, _, _ = s.mayReview(ctx, c, policy, who)
	} else {
		canReview = !s.ssoEnforced(ctx) // open bootstrap mode names the reviewer
	}
	threads, err := s.store.ChangeThreads(ctx, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to load comments")
		return
	}
	queue, err := s.changeQueue(ctx, d, policy)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to load the merge queue")
		return
	}
	canWrite := s.canOperate(r, d.Repo)
	caps := vcs.CapabilitiesOf(d.Provider)
	writeJSONCached(w, r, map[string]any{
		"change": d, "capabilities": caps, "gate": gate, "actions": actions, "threads": threads, "queue": queue,
		"permissions": map[string]bool{"can_review": canReview, "can_merge": canWrite, "can_comment": canWrite,
			"can_edit": canWrite && caps.NativeChanges},
	})
}

// providerCapabilities lists what each host supports natively.
func (s *Server) providerCapabilities(w http.ResponseWriter, r *http.Request) {
	out := map[string]vcs.Capabilities{}
	for _, name := range vcs.Names() {
		out[name] = vcs.CapabilitiesOf(name)
	}
	writeJSONCached(w, r, out)
}

// ---- live updates ----

// changeHub fans change notifications out to SSE subscribers. One LISTEN
// connection per server instance runs while anyone is subscribed.
type changeHub struct {
	mu     sync.Mutex
	subs   map[chan string]struct{}
	cancel context.CancelFunc
}

func (s *Server) subscribeChanges() (chan string, func()) {
	h := &s.changes
	ch := make(chan string, 16)
	h.mu.Lock()
	if h.subs == nil {
		h.subs = map[chan string]struct{}{}
	}
	h.subs[ch] = struct{}{}
	if h.cancel == nil {
		ctx, cancel := context.WithCancel(context.Background())
		h.cancel = cancel
		go s.listenChanges(ctx)
	}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs, ch)
		if len(h.subs) == 0 && h.cancel != nil {
			h.cancel()
			h.cancel = nil
		}
		h.mu.Unlock()
	}
}

// listenChanges holds the LISTEN until ctx ends, reconnecting on errors.
func (s *Server) listenChanges(ctx context.Context) {
	for ctx.Err() == nil {
		err := s.store.ListenChanges(ctx, func(id string) {
			h := &s.changes
			h.mu.Lock()
			for ch := range h.subs {
				select {
				case ch <- id:
				default: // a slow client misses an event, then refetches on the next
				}
			}
			h.mu.Unlock()
		})
		if ctx.Err() == nil {
			slog.Warn("code review: change listener stopped, reconnecting", "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
		}
	}
}

// sseKeepAlive keeps idle proxies from closing the stream.
const sseKeepAlive = 25 * time.Second

// streamChanges is GET /api/v1/changes/stream: one "change" event (data:
// {"id": N}) per change update, so pages refetch just that object.
func (s *Server) streamChanges(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	ch, unsubscribe := s.subscribeChanges()
	defer unsubscribe()
	fmt.Fprint(w, "event: ready\ndata: {}\n\n")
	flusher.Flush()
	ping := time.NewTicker(sseKeepAlive)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case id := <-ch:
			if _, err := fmt.Fprintf(w, "event: change\ndata: {\"id\":%s}\n\n", id); err != nil {
				return
			}
			flusher.Flush()
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// ---- metrics ----

// codeReviewStats is what the About page and /metrics show.
type codeReviewStats struct {
	WindowHours      int                       `json:"window_hours"`
	Calls            []store.ProviderCallCount `json:"calls"`
	TotalCalls       int64                     `json:"total_calls"`
	NotModified      int64                     `json:"not_modified"`
	NotModifiedRatio float64                   `json:"not_modified_ratio"`
	ReadPathCalls    int64                     `json:"read_path_calls"`
	OpenChanges      int                       `json:"open_changes"`
	CallsPerChange   float64                   `json:"calls_per_open_change"`
	Budgets          []store.ProviderBudget    `json:"budgets"`
	RefreshBacklog   int                       `json:"refresh_backlog"`
	OldestRefreshS   float64                   `json:"oldest_refresh_seconds"`
	Diffs            store.DiffStats           `json:"diffs"`
	DiffOpenP95MS    float64                   `json:"diff_open_p95_ms"`
	DiffOpenSamples  int                       `json:"diff_open_samples"`
	OutboxBacklog    int                       `json:"outbox_backlog"`
	OldestOutboxS    float64                   `json:"oldest_outbox_seconds"`
	Gates            store.GateStats           `json:"gates"`
}

func (s *Server) loadCodeReviewStats(ctx context.Context) (*codeReviewStats, error) {
	st := &codeReviewStats{WindowHours: 24}
	var err error
	if st.Calls, err = s.store.ProviderCallsSince(ctx, time.Now().Add(-24*time.Hour)); err != nil {
		return nil, err
	}
	for _, c := range st.Calls {
		if c.ReadPath {
			st.ReadPathCalls += c.Calls
			continue
		}
		st.TotalCalls += c.Calls
		if c.Result == "not_modified" {
			st.NotModified += c.Calls
		}
	}
	if st.TotalCalls > 0 {
		st.NotModifiedRatio = float64(st.NotModified) / float64(st.TotalCalls)
	}
	if st.OpenChanges, err = s.store.OpenChangeCount(ctx); err != nil {
		return nil, err
	}
	if st.OpenChanges > 0 {
		st.CallsPerChange = float64(st.TotalCalls) / float64(st.OpenChanges)
	}
	if st.Budgets, err = s.store.ListProviderBudgets(ctx); err != nil {
		return nil, err
	}
	n, oldest, err := s.store.RefreshBacklog(ctx)
	if err != nil {
		return nil, err
	}
	st.RefreshBacklog, st.OldestRefreshS = n, oldest.Seconds()
	n, oldest, err = s.store.OutboxBacklog(ctx)
	if err != nil {
		return nil, err
	}
	st.OutboxBacklog, st.OldestOutboxS = n, oldest.Seconds()
	if st.Gates, err = s.store.GateStatsSince(ctx, time.Now().Add(-24*time.Hour)); err != nil {
		return nil, err
	}
	if st.Diffs, err = s.store.DiffStatsSince(ctx, time.Now().Add(-24*time.Hour)); err != nil {
		return nil, err
	}
	p95, n := s.diffOpens.p95()
	st.DiffOpenP95MS, st.DiffOpenSamples = float64(p95.Microseconds())/1000, n
	return st, nil
}

// codeReviewMetrics is the About page's code-review card.
func (s *Server) codeReviewMetrics(w http.ResponseWriter, r *http.Request) {
	st, err := s.loadCodeReviewStats(r.Context())
	if err != nil {
		slog.Error("code review metrics", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to load code review metrics")
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// renderCodeReviewMetrics is the Prometheus text for /api/v1/metrics. The
// counts come from Postgres so they are the same on every replica and
// survive scale-to-zero; they are 24h windows, hence gauges.
func renderCodeReviewMetrics(st *codeReviewStats) string {
	var b strings.Builder
	b.WriteString("# HELP forge_provider_calls_24h Provider API calls in the last 24h, by provider, class and result.\n")
	b.WriteString("# TYPE forge_provider_calls_24h gauge\n")
	calls := append([]store.ProviderCallCount(nil), st.Calls...)
	sort.Slice(calls, func(i, j int) bool {
		return calls[i].Provider+calls[i].Class+calls[i].Result < calls[j].Provider+calls[j].Class+calls[j].Result
	})
	for _, c := range calls {
		if !c.ReadPath {
			fmt.Fprintf(&b, "forge_provider_calls_24h{provider=%q,class=%q,result=%q} %d\n", c.Provider, c.Class, c.Result, c.Calls)
		}
	}
	b.WriteString("# HELP forge_provider_read_path_calls_24h Provider calls attempted while serving a page (refused). Must be 0.\n")
	fmt.Fprintf(&b, "# TYPE forge_provider_read_path_calls_24h gauge\nforge_provider_read_path_calls_24h %d\n", st.ReadPathCalls)
	b.WriteString("# HELP forge_provider_not_modified_ratio_24h Share of provider calls answered 304 (free on GitHub).\n")
	fmt.Fprintf(&b, "# TYPE forge_provider_not_modified_ratio_24h gauge\nforge_provider_not_modified_ratio_24h %.4f\n", st.NotModifiedRatio)
	b.WriteString("# HELP forge_provider_calls_per_open_change_24h Provider calls in 24h per open change.\n")
	fmt.Fprintf(&b, "# TYPE forge_provider_calls_per_open_change_24h gauge\nforge_provider_calls_per_open_change_24h %.2f\n", st.CallsPerChange)
	b.WriteString("# HELP forge_provider_budget_remaining Calls left in a credential's budget window.\n# TYPE forge_provider_budget_remaining gauge\n")
	for _, bu := range st.Budgets {
		fmt.Fprintf(&b, "forge_provider_budget_remaining{credential=%q,provider=%q} %d\n", bu.Credential, bu.Provider, bu.Remaining)
	}
	b.WriteString("# HELP forge_provider_budget_capacity A credential's budget per window (the host's limit when it says).\n# TYPE forge_provider_budget_capacity gauge\n")
	for _, bu := range st.Budgets {
		fmt.Fprintf(&b, "forge_provider_budget_capacity{credential=%q,provider=%q} %d\n", bu.Credential, bu.Provider, bu.Capacity)
	}
	fmt.Fprintf(&b, "# HELP forge_changes_open Open changes across all repos.\n# TYPE forge_changes_open gauge\nforge_changes_open %d\n", st.OpenChanges)
	fmt.Fprintf(&b, "# HELP forge_change_refresh_backlog Change refreshes due now.\n# TYPE forge_change_refresh_backlog gauge\nforge_change_refresh_backlog %d\n", st.RefreshBacklog)
	fmt.Fprintf(&b, "# HELP forge_change_refresh_oldest_seconds Age of the oldest due change refresh.\n# TYPE forge_change_refresh_oldest_seconds gauge\nforge_change_refresh_oldest_seconds %.0f\n", st.OldestRefreshS)
	fmt.Fprintf(&b, "# HELP forge_provider_outbox_backlog Provider writes (gate publishes, merges) waiting to be sent.\n# TYPE forge_provider_outbox_backlog gauge\nforge_provider_outbox_backlog %d\n", st.OutboxBacklog)
	fmt.Fprintf(&b, "# HELP forge_provider_outbox_oldest_seconds Age of the oldest unsent provider write.\n# TYPE forge_provider_outbox_oldest_seconds gauge\nforge_provider_outbox_oldest_seconds %.0f\n", st.OldestOutboxS)
	fmt.Fprintf(&b, "# HELP forge_gate_evaluations_24h Merge-gate verdicts that changed in the last 24h.\n# TYPE forge_gate_evaluations_24h gauge\nforge_gate_evaluations_24h %d\n", st.Gates.Evaluations)
	b.WriteString("# HELP forge_gate_blocked_changes Open changes a merge rule is holding, by rule.\n# TYPE forge_gate_blocked_changes gauge\n")
	rules := make([]string, 0, len(st.Gates.BlockedBy))
	for id := range st.Gates.BlockedBy {
		rules = append(rules, id)
	}
	sort.Strings(rules)
	for _, id := range rules {
		fmt.Fprintf(&b, "forge_gate_blocked_changes{rule=%q} %d\n", id, st.Gates.BlockedBy[id])
	}
	b.WriteString("# HELP forge_diff_reads_24h Diff opens in 24h: hit (ready), miss (queued for a runner), pending, failed.\n# TYPE forge_diff_reads_24h gauge\n")
	results := make([]string, 0, len(st.Diffs.Reads))
	for r := range st.Diffs.Reads {
		results = append(results, r)
	}
	sort.Strings(results)
	for _, r := range results {
		fmt.Fprintf(&b, "forge_diff_reads_24h{result=%q} %d\n", r, st.Diffs.Reads[r])
	}
	fmt.Fprintf(&b, "# HELP forge_diff_cache_hit_ratio_24h Share of diff opens served ready (target > 0.99).\n# TYPE forge_diff_cache_hit_ratio_24h gauge\nforge_diff_cache_hit_ratio_24h %.4f\n", st.Diffs.HitRatio)
	fmt.Fprintf(&b, "# HELP forge_diff_compute_p95_ms Runner diff compute time p95 over 24h.\n# TYPE forge_diff_compute_p95_ms gauge\nforge_diff_compute_p95_ms %.0f\n", st.Diffs.ComputeP95MS)
	fmt.Fprintf(&b, "# HELP forge_diffs_computed_24h Diffs computed in 24h.\n# TYPE forge_diffs_computed_24h gauge\nforge_diffs_computed_24h %d\n", st.Diffs.Computed)
	fmt.Fprintf(&b, "# HELP forge_mirror_fetches_24h Diffs that needed a git fetch into the runner's mirror (git protocol, not an API call).\n# TYPE forge_mirror_fetches_24h gauge\nforge_mirror_fetches_24h %d\n", st.Diffs.Fetches)
	fmt.Fprintf(&b, "# HELP forge_diff_stored_bytes Bytes of diffs in the blob store.\n# TYPE forge_diff_stored_bytes gauge\nforge_diff_stored_bytes %d\n", st.Diffs.StoredBytes)
	fmt.Fprintf(&b, "# HELP forge_runner_task_backlog Runner tasks (diffs) waiting or running.\n# TYPE forge_runner_task_backlog gauge\nforge_runner_task_backlog %d\n", st.Diffs.TaskBacklog)
	fmt.Fprintf(&b, "# HELP forge_diff_open_p95_ms Diff open latency p95 on this instance (last 1000 opens; target < 300).\n# TYPE forge_diff_open_p95_ms gauge\nforge_diff_open_p95_ms %.1f\n", st.DiffOpenP95MS)
	return b.String()
}
