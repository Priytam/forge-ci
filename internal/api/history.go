package api

import (
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/store"
)

// History and health endpoints (batch 16): runner history, webhook log.

func (s *Server) registerHistoryRoutes() {
	m := s.mux
	m.HandleFunc("GET /api/v1/runner-history", s.runnerHistory)
	m.HandleFunc("GET /api/v1/runner-history/jobs", s.runnerHistoryJobs)
	m.HandleFunc("GET /api/v1/webhook-log", s.webhookLog)
	m.HandleFunc("GET /api/v1/webhook-log/failing", s.failingWebhooks)
}

func parseRangeQuery(r *http.Request) (store.Range, error) {
	q := r.URL.Query()
	return store.ParseRange(q.Get("range"), q.Get("from"), q.Get("to"), q.Get("bucket"), time.Now())
}

// runnerHistory: ?range=24h|7d|30d|custom (&from&to) &bucket= &runner= &repo= &job=
func (s *Server) runnerHistory(w http.ResponseWriter, r *http.Request) {
	rg, err := parseRangeQuery(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	q := r.URL.Query()
	f := store.HistoryFilter{Runner: q.Get("runner"), Repo: q.Get("repo"), Job: q.Get("job")}
	buckets, summary, err := s.store.RunnerHistory(r.Context(), rg, f)
	if err != nil {
		slog.Error("runner history", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to load history")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"range":  map[string]any{"from": rg.From, "to": rg.To, "bucket_seconds": int(rg.Bucket.Seconds()), "label": rg.Label},
		"filter": f, "buckets": buckets, "summary": summary,
	})
}

// runnerHistoryJobs lists the jobs behind one bucket: ?from=&to= (RFC 3339) plus the filter.
func (s *Server) runnerHistoryJobs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from, err1 := time.Parse(time.RFC3339, q.Get("from"))
	to, err2 := time.Parse(time.RFC3339, q.Get("to"))
	if err1 != nil || err2 != nil || !to.After(from) {
		writeErr(w, http.StatusBadRequest, "from and to (RFC 3339) are required")
		return
	}
	f := store.HistoryFilter{Runner: q.Get("runner"), Repo: q.Get("repo"), Job: q.Get("job")}
	limit := 50
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 && n <= 500 {
		limit = n
	}
	jobs, err := s.store.JobsInBucket(r.Context(), from, to, f, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to list jobs")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs})
}

// webhookLog: ?repo= (required) — recent deliveries and a 24h summary.
func (s *Server) webhookLog(w http.ResponseWriter, r *http.Request) {
	repo := r.URL.Query().Get("repo")
	if repo == "" {
		writeErr(w, http.StatusBadRequest, "repo query parameter is required")
		return
	}
	limit := 50
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 && n <= 500 {
		limit = n
	}
	health, entries, err := s.store.RepoWebhookHealth(r.Context(), repo, 24*time.Hour, limit)
	if err != nil {
		slog.Error("webhook log", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to load webhook log")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"health": health, "entries": entries})
}

// failingWebhooks (admin): repos with rejected or errored deliveries in 24h.
func (s *Server) failingWebhooks(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	repos, err := s.store.FailingWebhookRepos(r.Context(), 24*time.Hour)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to load webhook health")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"repos": repos})
}
