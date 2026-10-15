package api

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/priytamjeepandey/forge-ci/internal/store"
)

// Org-wide views for many repos and several environments: the environment
// board (one row per repo, one column per environment) and the job list
// across every pipeline. Both filter and page on the server.

func (s *Server) registerOrgViewRoutes() {
	m := s.mux
	m.HandleFunc("GET /api/v1/environment-board", s.environmentBoard)
	m.HandleFunc("GET /api/v1/jobs", s.listJobs)
}

// environmentBoard serves the org-wide board. Query: ?q (repo name contains),
// ?env (only repos active in that environment), ?attention=1 (only repos
// whose latest deploy attempt somewhere failed, is blocked or was canceled),
// ?limit/?offset. Environments come back in promotion order (dev → prod).
func (s *Server) environmentBoard(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, offset := pageParams(r)
	envs, err := s.store.OrgEnvironments(r.Context())
	if err != nil {
		slog.Error("org environments", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to list environments")
		return
	}
	rows, total, err := s.store.EnvMatrix(r.Context(), store.EnvMatrixFilter{
		Q:         q.Get("q"),
		Env:       strings.TrimSpace(q.Get("env")),
		Attention: q.Get("attention") == "1",
	}, limit, offset)
	if err != nil {
		slog.Error("environment board", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to load the environment board")
		return
	}
	w.Header().Set("X-Total-Count", strconv.Itoa(total))
	w.Header().Set("X-Has-More", strconv.FormatBool(offset+len(rows) < total))
	writeJSON(w, http.StatusOK, map[string]any{"environments": envs, "rows": rows})
}

// listJobs lists jobs across all pipelines, newest first. Filters (AND):
// ?repo (repeatable or comma-separated), ?ref, ?status (repeatable or
// comma-separated job statuses), ?runner, ?environment, ?name (contains).
func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, offset := pageParams(r)
	f := store.JobFilter{
		Repos:       queryList(r, "repo"),
		Ref:         strings.TrimSpace(q.Get("ref")),
		Statuses:    queryList(r, "status"),
		Runner:      strings.TrimSpace(q.Get("runner")),
		Environment: strings.TrimSpace(q.Get("environment")),
		Name:        strings.TrimSpace(q.Get("name")),
	}
	if err := f.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	jobs, total, err := s.store.ListJobsFiltered(r.Context(), f, limit, offset)
	if err != nil {
		slog.Error("list jobs", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to list jobs")
		return
	}
	w.Header().Set("X-Total-Count", strconv.Itoa(total))
	w.Header().Set("X-Has-More", strconv.FormatBool(offset+len(jobs) < total))
	writeJSON(w, http.StatusOK, jobs)
}
