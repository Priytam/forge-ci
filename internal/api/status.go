package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/buildinfo"
	"github.com/priytamjeepandey/forge-ci/internal/store"
)

// Server status (public, auth-exempt): what version this is and whether it
// is in maintenance, so every page can show the banner and the runner
// install line can say which version it hands out. Maintenance mode (admin)
// pauses job acquisition on every runner — running jobs finish, new ones
// queue — for a server upgrade.

var startedAt = time.Now()

func (s *Server) registerStatusRoutes() {
	m := s.mux
	m.HandleFunc("GET /api/v1/status", s.serverStatus)
	m.HandleFunc("GET /api/v1/about", s.about)
	m.HandleFunc("PUT /api/v1/maintenance", s.setMaintenance)
}

func (s *Server) serverStatus(w http.ResponseWriter, r *http.Request) {
	mm, _ := s.store.Maintenance(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"version":     buildinfo.Version,
		"maintenance": mm,
	})
}

// bundledRunnerVersion is the version of the runner binaries served from
// /downloads: the same build as the server (deploy/Dockerfile.server).
func bundledRunnerVersion() string {
	if b, err := os.ReadFile(filepath.Join(downloadsDir(), "VERSION")); err == nil {
		return strings.TrimSpace(string(b))
	}
	return buildinfo.Version
}

// about is the admin "About" page: versions, migrations, uptime, maintenance.
func (s *Server) about(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	mm, _ := s.store.Maintenance(r.Context())
	schema, _ := s.store.SchemaStatus(r.Context())
	dbOK := s.store.Ping(r.Context()) == nil
	writeJSON(w, http.StatusOK, map[string]any{
		"version":          buildinfo.Version,
		"platform":         buildinfo.Platform,
		"runner_version":   bundledRunnerVersion(),
		"started_at":       startedAt,
		"uptime_seconds":   int(time.Since(startedAt).Seconds()),
		"database_ok":      dbOK,
		"schema":           schema,
		"maintenance":      mm,
		"downloads_served": downloadsServed(),
	})
}

func downloadsServed() []string {
	out := []string{}
	for name := range downloadableFiles {
		if _, err := os.Stat(filepath.Join(downloadsDir(), name)); err == nil {
			out = append(out, name)
		}
	}
	return out
}

func (s *Server) setMaintenance(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var req struct {
		On      bool   `json:"on"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if len(req.Message) > 300 {
		req.Message = req.Message[:300]
	}
	mm, err := s.store.SetMaintenance(r.Context(), store.Maintenance{On: req.On, Message: strings.TrimSpace(req.Message), By: s.actorLabel(r)})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to save")
		return
	}
	s.audit(r, "maintenance.set", "", "", "ok", map[string]any{"on": req.On, "message": req.Message})
	writeJSON(w, http.StatusOK, mm)
}
