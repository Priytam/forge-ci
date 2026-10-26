package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/store"
)

// Runner operations (admin): drain, resume, upgrade one, upgrade all.

func (s *Server) registerRunnerOpRoutes() {
	m := s.mux
	m.HandleFunc("POST /api/v1/runners/{id}/drain", s.drainRunner)
	m.HandleFunc("POST /api/v1/runners/{id}/resume", s.resumeRunner)
	m.HandleFunc("POST /api/v1/runners/{id}/upgrade", s.upgradeRunner)
	m.HandleFunc("GET /api/v1/runners/rollout", s.getRollout)
	m.HandleFunc("POST /api/v1/runners/rollout", s.startRollout)
	m.HandleFunc("DELETE /api/v1/runners/rollout", s.stopRollout)
	m.HandleFunc("DELETE /api/v1/runners/{id}", s.deleteRunner)
}

func (s *Server) runnerAction(w http.ResponseWriter, r *http.Request, action string, fn func(id string) error) {
	if !s.requireAdmin(w, r) {
		return
	}
	id := r.PathValue("id")
	if err := fn(id); errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "runner not found")
		return
	} else if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to "+action)
		return
	}
	s.audit(r, "runner."+action, id, "", "ok", nil)
	running, _ := s.store.RunnerRunningJobs(r.Context(), id)
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "action": action, "running_jobs": running})
}

func (s *Server) drainRunner(w http.ResponseWriter, r *http.Request) {
	s.runnerAction(w, r, "drain", func(id string) error { return s.store.SetRunnerDraining(r.Context(), id, true) })
}

func (s *Server) resumeRunner(w http.ResponseWriter, r *http.Request) {
	s.runnerAction(w, r, "resume", func(id string) error { return s.store.SetRunnerDraining(r.Context(), id, false) })
}

func (s *Server) upgradeRunner(w http.ResponseWriter, r *http.Request) {
	s.runnerAction(w, r, "upgrade", func(id string) error { return s.store.RequestRunnerUpdate(r.Context(), id) })
}

// deleteRunner removes a runner that is offline and idle (a stale
// registration); an online or busy runner is refused with 409.
func (s *Server) deleteRunner(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id := r.PathValue("id")
	switch err := s.store.DeleteRunner(r.Context(), id); {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "runner not found")
		return
	case errors.Is(err, store.ErrRunnerActive):
		writeErr(w, http.StatusConflict, "runner is online or running jobs; drain and stop it first")
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "failed to remove runner")
		return
	}
	s.audit(r, "runner.remove", id, "", "ok", nil)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) getRollout(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	ro, err := s.store.Rollout(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to load rollout")
		return
	}
	writeJSON(w, http.StatusOK, ro)
}

// startRollout upgrades every online runner whose version differs from the
// server's bundled runner, one at a time (drain → update → resume).
func (s *Server) startRollout(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	target := bundledRunnerVersion()
	if target == "" || target == "dev" {
		writeErr(w, http.StatusBadRequest, "this server has no stamped version to upgrade runners to (build with -ldflags -X …/buildinfo.Version=…)")
		return
	}
	ro, err := s.store.StartRollout(r.Context(), target, s.actorLabel(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to start rollout")
		return
	}
	// First step now, the scheduler continues every tick.
	if _, err := s.store.AdvanceRollout(r.Context(), 30*time.Second); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to start rollout: "+err.Error())
		return
	}
	s.audit(r, "runner.rollout.start", target, "", "ok", nil)
	ro, _ = s.store.Rollout(r.Context())
	writeJSON(w, http.StatusOK, ro)
}

func (s *Server) stopRollout(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if err := s.store.StopRollout(r.Context()); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to stop rollout")
		return
	}
	s.audit(r, "runner.rollout.stop", "", "", "ok", nil)
	w.WriteHeader(http.StatusNoContent)
}
