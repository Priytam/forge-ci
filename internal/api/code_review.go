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
		"GET /api/v1/changes":                  s.listChanges,
		"GET /api/v1/changes/{id}":             s.getChange,
		"GET /api/v1/changes/stream":           s.streamChanges,
		"GET /api/v1/code-review/metrics":      s.codeReviewMetrics,
		"GET /api/v1/code-review/capabilities": s.providerCapabilities,
	}
}

func (s *Server) registerCodeReviewRoutes() {
	for pattern, h := range s.changeReadRoutes() {
		s.mux.HandleFunc(pattern, readPath(h))
	}
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
	writeJSONCached(w, r, map[string]any{"change": d, "capabilities": vcs.CapabilitiesOf(d.Provider)})
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
	return b.String()
}
