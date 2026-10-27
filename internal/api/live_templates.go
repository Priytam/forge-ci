package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/priytamjeepandey/forge-ci/internal/store"
)

// Forge Live templates and the fleet (phase D). A template declares one
// application on many targets, so managing templates is for admins; the
// fleet view is a read route like every other live page.

// liveFleet is GET /api/v1/live/fleet: a page of applications by name, each
// with its status on every target (the columns).
func (s *Server) liveFleet(w http.ResponseWriter, r *http.Request) {
	q := store.FleetQuery{Environment: r.URL.Query().Get("environment"), Template: r.URL.Query().Get("template"),
		Search: r.URL.Query().Get("q"), Limit: 200}
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
		q.Limit = min(n, 1000)
	}
	if n, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && n > 0 {
		q.Offset = n
	}
	rows, total, targets, err := s.store.LiveFleet(r.Context(), q)
	if err != nil {
		slog.Error("live: fleet", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to load the fleet")
		return
	}
	cols := make([]liveAppTarget, 0, len(targets))
	for _, t := range targets {
		cols = append(cols, liveAppTarget{ID: t.ID, Name: t.Name, Environment: t.Environment, Provider: t.Provider,
			Capabilities: t.Capabilities, AgentVersion: t.AgentVersion, LastSeenAt: t.LastSeenAt, Synced: t.Synced})
	}
	writeJSONCached(w, r, map[string]any{"rows": rows, "total": total, "targets": cols})
}

func (s *Server) listLiveTemplates(w http.ResponseWriter, r *http.Request) {
	ts, err := s.store.LiveTemplates(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to list templates")
		return
	}
	writeJSONCached(w, r, ts)
}

// saveLiveTemplate is POST /api/v1/live/templates and PUT …/{id}.
func (s *Server) saveLiveTemplate(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var t store.LiveTemplate
	if err := json.NewDecoder(r.Body).Decode(&t); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	t.ID = 0
	if err := t.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	status := http.StatusCreated
	if r.Method == http.MethodPut {
		id, ok := pathID(r)
		if !ok {
			writeErr(w, http.StatusBadRequest, "invalid template id")
			return
		}
		t.ID, status = id, http.StatusOK
	}
	err := s.store.SaveLiveTemplate(r.Context(), &t, s.actorLabel(r))
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "template not found")
		return
	case errors.Is(err, store.ErrDuplicateTemplate):
		writeErr(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		slog.Error("live: save template", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to save the template")
		return
	}
	s.audit(r, "live.template", t.Name, "", "saved", map[string]any{"id": t.ID, "path": t.Path, "namespace": t.Namespace,
		"targets": t.Targets, "environments": t.Environments, "auto_sync": t.AutoSync, "prune": t.Prune, "self_heal": t.SelfHeal})
	writeJSON(w, status, t)
}

func (s *Server) deleteLiveTemplate(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid template id")
		return
	}
	t, err := s.store.LiveTemplate(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "template not found")
		return
	}
	if err == nil {
		err = s.store.DeleteLiveTemplate(r.Context(), id)
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to delete the template")
		return
	}
	s.audit(r, "live.template", t.Name, "", "deleted", map[string]any{"id": id})
	w.WriteHeader(http.StatusNoContent)
}

// liveAgentApps is GET /api/v1/live/agent/apps: the applications the
// agent's target's templates declare.
func (s *Server) liveAgentApps(w http.ResponseWriter, r *http.Request) {
	t, ok := s.liveAgent(w, r)
	if !ok {
		return
	}
	apps, err := s.store.LiveDeclaredApps(r.Context(), t)
	if err != nil {
		slog.Error("live: declared apps", "target", t.ID, "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to list declared applications")
		return
	}
	writeJSON(w, http.StatusOK, apps)
}
