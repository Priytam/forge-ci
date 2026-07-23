package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
	"github.com/priytamjeepandey/forge-ci/internal/store"
)

var keyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func (s *Server) registerSettingsRoutes() {
	m := s.mux

	// Variables (GitLab Settings -> CI/CD -> Variables).
	m.HandleFunc("GET /api/v1/variables", s.listVariables)
	m.HandleFunc("POST /api/v1/variables", s.createVariable)
	m.HandleFunc("PUT /api/v1/variables/{id}", s.updateVariable)
	m.HandleFunc("DELETE /api/v1/variables/{id}", s.deleteVariable)

	// Runners.
	m.HandleFunc("GET /api/v1/runners", s.listRunners)
	m.HandleFunc("POST /api/v1/runners/{rid}/pause", s.pauseRunner)

	// Artifacts.
	m.HandleFunc("GET /api/v1/artifacts", s.listArtifacts)
	m.HandleFunc("GET /api/v1/artifacts/{id}/download", s.downloadArtifact)
	m.HandleFunc("POST /api/v1/runner/jobs/{id}/artifacts", s.uploadArtifact)

	// Repo settings (runner-group selection by tags).
	m.HandleFunc("GET /api/v1/repo-settings", s.getRepoSettings)
	m.HandleFunc("PUT /api/v1/repo-settings", s.putRepoSettings)

	// Members & approval rules (RBAC).
	m.HandleFunc("GET /api/v1/members", s.listMembers)
	m.HandleFunc("POST /api/v1/members", s.addMember)
	m.HandleFunc("DELETE /api/v1/members/{id}", s.removeMember)
	m.HandleFunc("GET /api/v1/protected-environments", s.listProtectedEnvs)
	m.HandleFunc("POST /api/v1/protected-environments", s.upsertProtectedEnv)
}

// ---- variables ----

func validateVariable(req *proto.VariableRequest) error {
	if req.Repo == "" {
		return errors.New("repo is required")
	}
	if !keyRe.MatchString(req.Key) {
		return errors.New("key must be a valid env var name ([A-Za-z_][A-Za-z0-9_]*)")
	}
	if req.EnvironmentScope == "" {
		req.EnvironmentScope = "*"
	}
	if req.Masked {
		if len(req.Value) < 8 {
			return errors.New("masked values must be at least 8 characters")
		}
		if strings.ContainsAny(req.Value, " \t\n\r") {
			return errors.New("masked values must not contain whitespace")
		}
	}
	return nil
}

func (s *Server) listVariables(w http.ResponseWriter, r *http.Request) {
	repo := r.URL.Query().Get("repo")
	if repo == "" {
		writeErr(w, http.StatusBadRequest, "repo query param is required")
		return
	}
	reveal := r.URL.Query().Get("reveal") == "1"
	vars, err := s.store.ListVariables(r.Context(), repo, reveal)
	if err != nil {
		slog.Error("list variables", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to list variables")
		return
	}
	writeJSON(w, http.StatusOK, vars)
}

func (s *Server) createVariable(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var req proto.VariableRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if err := validateVariable(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	v, err := s.store.CreateVariable(r.Context(), req)
	if errors.Is(err, store.ErrDuplicateVariable) {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		slog.Error("create variable", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to create variable")
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

func (s *Server) updateVariable(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid variable id")
		return
	}
	var req proto.VariableRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req.Repo, req.Key = "-", "PLACEHOLDER" // repo/key are immutable on update
	if err := validateVariable(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	v, err := s.store.UpdateVariable(r.Context(), id, req)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "variable not found")
		return
	}
	if err != nil {
		slog.Error("update variable", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to update variable")
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) deleteVariable(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid variable id")
		return
	}
	err := s.store.DeleteVariable(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "variable not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to delete variable")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- runners ----

func (s *Server) listRunners(w http.ResponseWriter, r *http.Request) {
	runners, err := s.store.ListRunners(r.Context())
	if err != nil {
		slog.Error("list runners", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to list runners")
		return
	}
	writeJSON(w, http.StatusOK, runners)
}

func (s *Server) pauseRunner(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var req struct {
		Paused bool `json:"paused"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	runner, err := s.store.SetRunnerPaused(r.Context(), r.PathValue("rid"), req.Paused)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "runner not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to update runner")
		return
	}
	writeJSON(w, http.StatusOK, runner)
}

// ---- artifacts ----

func (s *Server) uploadArtifact(w http.ResponseWriter, r *http.Request) {
	if !s.requireRunnerAuth(w, r) {
		return
	}
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid job id")
		return
	}
	name := r.URL.Query().Get("name")
	if name == "" {
		name = "artifacts.tar.gz"
	}
	key := fmt.Sprintf("job-%d/%s", id, name)
	// Enforce MAX_ARTIFACT_BYTES: read one byte past the cap so an oversize
	// upload can be detected, then reject with 413 and clean up the partial
	// blob. A cap of 0 disables the limit.
	body := r.Body
	if s.maxArtifactBytes > 0 {
		body = io.NopCloser(io.LimitReader(r.Body, s.maxArtifactBytes+1))
	}
	size, err := s.blobs.Put(r.Context(), key, body)
	if err != nil {
		slog.Error("store artifact blob", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to store artifact")
		return
	}
	if s.maxArtifactBytes > 0 && size > s.maxArtifactBytes {
		if derr := s.blobs.Delete(r.Context(), key); derr != nil {
			slog.Error("cleanup oversize artifact blob", "err", derr, "key", key)
		}
		writeErr(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("artifact exceeds MAX_ARTIFACT_BYTES (%d bytes)", s.maxArtifactBytes))
		return
	}
	a, err := s.store.SaveArtifact(r.Context(), id, name, key, size)
	if err != nil {
		slog.Error("save artifact meta", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to record artifact")
		return
	}
	writeJSON(w, http.StatusCreated, a)
}

func (s *Server) listArtifacts(w http.ResponseWriter, r *http.Request) {
	var jobID int64
	if v := r.URL.Query().Get("job"); v != "" {
		fmt.Sscanf(v, "%d", &jobID)
	}
	arts, err := s.store.ListArtifacts(r.Context(), r.URL.Query().Get("repo"), jobID)
	if err != nil {
		slog.Error("list artifacts", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to list artifacts")
		return
	}
	writeJSON(w, http.StatusOK, arts)
}

func (s *Server) downloadArtifact(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid artifact id")
		return
	}
	key, name, err := s.store.GetArtifactPath(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "artifact not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to load artifact")
		return
	}
	rc, err := s.blobs.Get(r.Context(), key)
	if err != nil {
		slog.Error("read artifact blob", "err", err, "key", key)
		writeErr(w, http.StatusInternalServerError, "failed to read artifact")
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	_, _ = io.Copy(w, rc)
}

// ---- repo settings ----

func (s *Server) getRepoSettings(w http.ResponseWriter, r *http.Request) {
	repo := r.URL.Query().Get("repo")
	if repo == "" {
		writeErr(w, http.StatusBadRequest, "repo query param is required")
		return
	}
	tags, err := s.store.GetRepoDefaultTags(r.Context(), repo)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to load repo settings")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"repo": repo, "default_runner_tags": tags})
}

func (s *Server) putRepoSettings(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var req struct {
		Repo              string   `json:"repo"`
		DefaultRunnerTags []string `json:"default_runner_tags"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Repo == "" {
		writeErr(w, http.StatusBadRequest, "repo is required")
		return
	}
	for _, t := range req.DefaultRunnerTags {
		if strings.TrimSpace(t) == "" {
			writeErr(w, http.StatusBadRequest, "tags must be non-empty strings")
			return
		}
	}
	if err := s.store.SetRepoDefaultTags(r.Context(), req.Repo, req.DefaultRunnerTags); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to save repo settings")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- members & approval rules ----

func (s *Server) listMembers(w http.ResponseWriter, r *http.Request) {
	repo := r.URL.Query().Get("repo")
	if repo == "" {
		writeErr(w, http.StatusBadRequest, "repo query param is required")
		return
	}
	members, err := s.store.ListMembers(r.Context(), repo)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to list members")
		return
	}
	writeJSON(w, http.StatusOK, members)
}

func (s *Server) addMember(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var req proto.Member
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Repo == "" || req.Username == "" {
		writeErr(w, http.StatusBadRequest, "repo and username are required")
		return
	}
	if req.Role != "admin" && req.Role != "owner" && req.Role != "developer" {
		writeErr(w, http.StatusBadRequest, "role must be admin, owner or developer")
		return
	}
	m, err := s.store.AddMember(r.Context(), req.Repo, req.Username, req.Role)
	if err != nil {
		slog.Error("add member", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to add member")
		return
	}
	writeJSON(w, http.StatusCreated, m)
}

func (s *Server) removeMember(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid member id")
		return
	}
	err := s.store.RemoveMember(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "member not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to remove member")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listProtectedEnvs(w http.ResponseWriter, r *http.Request) {
	repo := r.URL.Query().Get("repo")
	envs, err := s.store.ListProtectedEnvs(r.Context(), repo)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to list protected environments")
		return
	}
	writeJSON(w, http.StatusOK, envs)
}

func (s *Server) upsertProtectedEnv(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var pe proto.ProtectedEnvironment
	if err := json.NewDecoder(r.Body).Decode(&pe); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if pe.Name == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}
	if pe.RequiredApprovals < 1 {
		pe.RequiredApprovals = 1
	}
	if pe.ApprovalTimeoutHours < 1 {
		pe.ApprovalTimeoutHours = 24
	}
	out, err := s.store.UpsertProtectedEnv(r.Context(), pe)
	if err != nil {
		slog.Error("upsert protected env", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to save approval rule")
		return
	}
	writeJSON(w, http.StatusOK, out)
}
