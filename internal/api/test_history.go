package api

import (
	"log/slog"
	"net/http"
	"strconv"
)

// registerTestHistoryRoutes wires the per-repo test-case history behind the
// Tests tab. The data itself is populated as a side effect of the existing
// report_junit upload path (see junit.go's caseResult and settings.go's
// uploadReport) — this is read-only.
func (s *Server) registerTestHistoryRoutes() {
	s.mux.HandleFunc("GET /api/v1/test-history", s.listTestHistory)
	s.mux.HandleFunc("GET /api/v1/coverage", s.coverageTrend)
}

// coverageTrend returns a repo's recent pipeline coverage, oldest first
// (?repo= required, ?ref= optional, ?limit= default 30, max 200).
func (s *Server) coverageTrend(w http.ResponseWriter, r *http.Request) {
	repo := r.URL.Query().Get("repo")
	if repo == "" {
		writeErr(w, http.StatusBadRequest, "repo query parameter is required")
		return
	}
	limit := 30
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 && n <= 200 {
		limit = n
	}
	points, err := s.store.CoverageTrend(r.Context(), repo, r.URL.Query().Get("ref"), limit)
	if err != nil {
		slog.Error("coverage trend", "err", err, "repo", repo)
		writeErr(w, http.StatusInternalServerError, "failed to load coverage")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"repo": repo, "points": points})
}

// listTestHistory returns, for every test case Forge has recorded for ?repo,
// its most recent runs (newest first). Like listEnvironments, repo is a query
// parameter rather than a path segment because repo names contain slashes.
// ?limit caps runs PER CASE, not the total row count (see
// store.defaultCaseHistoryRuns / maxCaseHistoryRuns).
func (s *Server) listTestHistory(w http.ResponseWriter, r *http.Request) {
	repo := r.URL.Query().Get("repo")
	if repo == "" {
		writeErr(w, http.StatusBadRequest, "repo query parameter is required")
		return
	}
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	history, err := s.store.ListTestCaseHistory(r.Context(), repo, limit)
	if err != nil {
		slog.Error("list test history", "err", err, "repo", repo)
		writeErr(w, http.StatusInternalServerError, "failed to load test history")
		return
	}
	writeJSON(w, http.StatusOK, history)
}
