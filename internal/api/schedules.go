package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/cron"
	"github.com/priytamjeepandey/forge-ci/internal/store"
)

// registerScheduleRoutes wires the cron-scheduled-pipeline CRUD. Mirrors the
// settings routes: the list (GET) is open; mutations are admin-gated + audited.
func (s *Server) registerScheduleRoutes() {
	m := s.mux
	m.HandleFunc("GET /api/v1/schedules", s.listSchedules)
	m.HandleFunc("POST /api/v1/schedules", s.createSchedule)
	m.HandleFunc("PUT /api/v1/schedules/{id}", s.updateSchedule)
	m.HandleFunc("DELETE /api/v1/schedules/{id}", s.deleteSchedule)
}

// listSchedules returns the schedules for a repo (?repo=), or all schedules when
// repo is omitted.
func (s *Server) listSchedules(w http.ResponseWriter, r *http.Request) {
	schedules, err := s.store.ListSchedules(r.Context(), r.URL.Query().Get("repo"))
	if err != nil {
		slog.Error("list schedules", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to list schedules")
		return
	}
	writeJSON(w, http.StatusOK, schedules)
}

// createSchedule validates the cron expression, computes next_run_at (UTC) and
// inserts the schedule. Bad cron → 400.
func (s *Server) createSchedule(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var req struct {
		Repo    string `json:"repo"`
		Ref     string `json:"ref"`
		Cron    string `json:"cron"`
		Enabled *bool  `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req.Repo = strings.TrimSpace(req.Repo)
	req.Ref = strings.TrimSpace(req.Ref)
	req.Cron = strings.TrimSpace(req.Cron)
	if req.Repo == "" || req.Ref == "" || req.Cron == "" {
		writeErr(w, http.StatusBadRequest, "repo, ref and cron are required")
		return
	}
	now := time.Now().UTC()
	next, err := cron.Next(req.Cron, now)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	// Identity-from-session: when SSO is enforced the creator is the authenticated
	// identity; the client value is used only in open bootstrap mode.
	createdBy := s.sessionActor(r)
	sc, err := s.store.CreateSchedule(r.Context(), req.Repo, req.Ref, req.Cron, enabled, createdBy, next)
	if err != nil {
		slog.Error("create schedule", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to create schedule")
		return
	}
	s.audit(r, "schedule.create", strconv.FormatInt(sc.ID, 10), sc.Repo, "ok", map[string]any{
		"id":          sc.ID,
		"ref":         sc.Ref,
		"cron":        sc.Cron,
		"enabled":     sc.Enabled,
		"next_run_at": sc.NextRunAt,
	})
	writeJSON(w, http.StatusCreated, sc)
}

// updateSchedule applies partial updates (cron/ref/enabled) and recomputes
// next_run_at from the effective cron. Bad cron → 400.
func (s *Server) updateSchedule(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid schedule id")
		return
	}
	var req struct {
		Cron    *string `json:"cron"`
		Ref     *string `json:"ref"`
		Enabled *bool   `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	// Resolve the effective cron: the new value if provided, else the stored one,
	// so we can validate and recompute next_run_at consistently.
	existing, err := s.store.GetSchedule(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "schedule not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to load schedule")
		return
	}
	effectiveCron := existing.Cron
	if req.Cron != nil {
		effectiveCron = strings.TrimSpace(*req.Cron)
		req.Cron = &effectiveCron
	}
	now := time.Now().UTC()
	next, err := cron.Next(effectiveCron, now)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Ref != nil {
		trimmed := strings.TrimSpace(*req.Ref)
		if trimmed == "" {
			writeErr(w, http.StatusBadRequest, "ref must not be empty")
			return
		}
		req.Ref = &trimmed
	}
	sc, err := s.store.UpdateSchedule(r.Context(), id, req.Cron, req.Ref, req.Enabled, next)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "schedule not found")
		return
	}
	if err != nil {
		slog.Error("update schedule", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to update schedule")
		return
	}
	s.audit(r, "schedule.update", strconv.FormatInt(sc.ID, 10), sc.Repo, "ok", map[string]any{
		"id":          sc.ID,
		"ref":         sc.Ref,
		"cron":        sc.Cron,
		"enabled":     sc.Enabled,
		"next_run_at": sc.NextRunAt,
	})
	writeJSON(w, http.StatusOK, sc)
}

// deleteSchedule removes a schedule. Schedules are config, not run data — this is
// the only path that deletes them (retention GC leaves them alone).
func (s *Server) deleteSchedule(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid schedule id")
		return
	}
	err := s.store.DeleteSchedule(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "schedule not found")
		return
	}
	if err != nil {
		slog.Error("delete schedule", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to delete schedule")
		return
	}
	s.audit(r, "schedule.delete", strconv.FormatInt(id, 10), "", "ok", map[string]any{"id": id})
	w.WriteHeader(http.StatusNoContent)
}
