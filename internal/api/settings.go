package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
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

	// Test reports (JUnit). The runner POSTs XML; the server parses + stores it.
	m.HandleFunc("POST /api/v1/runner/jobs/{id}/report", s.uploadReport)
	m.HandleFunc("GET /api/v1/jobs/{id}/report", s.getReport)

	// Cache (runner restore/save; shared across pipelines per repo+key).
	m.HandleFunc("GET /api/v1/runner/jobs/{id}/cache", s.downloadCache)
	m.HandleFunc("POST /api/v1/runner/jobs/{id}/cache", s.uploadCache)

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
	// NEVER record the variable value.
	s.audit(r, "variable.create", v.Key, v.Repo, "ok", map[string]any{
		"key":               v.Key,
		"masked":            v.Masked,
		"protected":         v.Protected,
		"environment_scope": v.EnvironmentScope,
	})
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
	// NEVER record the variable value.
	s.audit(r, "variable.update", v.Key, v.Repo, "ok", map[string]any{
		"id":                v.ID,
		"key":               v.Key,
		"masked":            v.Masked,
		"protected":         v.Protected,
		"environment_scope": v.EnvironmentScope,
	})
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
	s.audit(r, "variable.delete", strconv.FormatInt(id, 10), "", "ok", map[string]any{"id": id})
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
	rid := r.PathValue("rid")
	runner, err := s.store.SetRunnerPaused(r.Context(), rid, req.Paused)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "runner not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to update runner")
		return
	}
	action := "runner.resume"
	if req.Paused {
		action = "runner.pause"
	}
	s.audit(r, action, rid, "", "ok", map[string]any{"paused": req.Paused})
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

// ---- test reports (JUnit) ----

// maxReportBytes caps a single report upload so a runaway XML can't exhaust
// memory. JUnit XML is text and small; 32 MiB is generous.
const maxReportBytes = 32 << 20

// uploadReport ingests a job's concatenated JUnit XML, parses it server-side,
// and stores a per-job summary. Malformed or empty XML is NOT an error: the
// server records no report and returns 200 {"parsed": false} so the runner
// never fails a successful job over a bad report.
func (s *Server) uploadReport(w http.ResponseWriter, r *http.Request) {
	if !s.requireRunnerAuth(w, r) {
		return
	}
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid job id")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxReportBytes))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "failed to read report body")
		return
	}
	report, parsed := parseJUnit(body, id)
	if !parsed {
		// Empty/malformed/zero-test XML: not a job failure, just no report.
		writeJSON(w, http.StatusOK, map[string]any{"parsed": false})
		return
	}
	if err := s.store.SaveJUnitReport(r.Context(), report); err != nil {
		slog.Error("save junit report", "err", err, "job", id)
		writeErr(w, http.StatusInternalServerError, "failed to store report")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"parsed": true, "total": report.Total, "failed": report.Failed, "skipped": report.Skipped,
	})
}

// getReport returns a job's parsed JUnit summary, or 404 when it has none.
func (s *Server) getReport(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid job id")
		return
	}
	report, err := s.store.GetJUnitReport(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "no test report for this job")
		return
	}
	if err != nil {
		slog.Error("get junit report", "err", err, "job", id)
		writeErr(w, http.StatusInternalServerError, "failed to load report")
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// ---- cache ----

// cacheBlobKey derives a deterministic, filesystem-safe blob key for a repo's
// cache under a given cache key. The repo segment is sanitized for readability;
// a sha256 of repo+key guarantees uniqueness (repo/key may contain slashes or
// other unsafe characters). Being deterministic, a repeat save for the same
// (repo, key) overwrites the same object.
func cacheBlobKey(repo, key string) string {
	sum := sha256.Sum256([]byte(repo + "\x00" + key))
	return "cache/" + sanitizeSegment(repo) + "/" + hex.EncodeToString(sum[:]) + ".tar.gz"
}

// sanitizeSegment reduces a string to a safe single path segment: characters
// outside [A-Za-z0-9._-] become '-'. Empty input becomes "_".
func sanitizeSegment(s string) string {
	if s == "" {
		return "_"
	}
	b := []byte(s)
	for i, c := range b {
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '.', c == '_', c == '-':
		default:
			b[i] = '-'
		}
	}
	return string(b)
}

// uploadCache stores a job's cache archive under repo+key. The repo is resolved
// server-side from the job so the runner cannot write outside its repo's cache
// namespace. Enforces MAX_CACHE_BYTES: an over-cap upload is rejected (413) and
// the partial blob cleaned up — the runner treats this as non-fatal and the job
// still succeeds.
func (s *Server) uploadCache(w http.ResponseWriter, r *http.Request) {
	if !s.requireRunnerAuth(w, r) {
		return
	}
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid job id")
		return
	}
	key := r.URL.Query().Get("key")
	if key == "" {
		writeErr(w, http.StatusBadRequest, "cache key is required")
		return
	}
	repo, err := s.store.RepoForJob(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "job not found")
		return
	}
	if err != nil {
		slog.Error("cache: resolve repo for job", "err", err, "job", id)
		writeErr(w, http.StatusInternalServerError, "failed to resolve job")
		return
	}
	blobKey := cacheBlobKey(repo, key)
	body := r.Body
	if s.maxCacheBytes > 0 {
		body = io.NopCloser(io.LimitReader(r.Body, s.maxCacheBytes+1))
	}
	size, err := s.blobs.Put(r.Context(), blobKey, body)
	if err != nil {
		slog.Error("store cache blob", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to store cache")
		return
	}
	if s.maxCacheBytes > 0 && size > s.maxCacheBytes {
		if derr := s.blobs.Delete(r.Context(), blobKey); derr != nil {
			slog.Error("cleanup oversize cache blob", "err", derr, "key", blobKey)
		}
		writeErr(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("cache exceeds MAX_CACHE_BYTES (%d bytes)", s.maxCacheBytes))
		return
	}
	if err := s.store.SaveCache(r.Context(), repo, key, blobKey, size); err != nil {
		slog.Error("save cache meta", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to record cache")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"repo": repo, "key": key, "size_bytes": size})
}

// downloadCache streams a job's cache archive. The runner passes the primary
// key as ?key= and any fallback keys as repeated ?fallback= params; the server
// tries them in order (exact key, then prefix/default) and returns the first
// hit. A miss is 404 (the runner logs "cache miss" and continues). The matched
// key is returned in the X-Cache-Key response header.
func (s *Server) downloadCache(w http.ResponseWriter, r *http.Request) {
	if !s.requireRunnerAuth(w, r) {
		return
	}
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid job id")
		return
	}
	key := r.URL.Query().Get("key")
	if key == "" {
		writeErr(w, http.StatusBadRequest, "cache key is required")
		return
	}
	keys := append([]string{key}, r.URL.Query()["fallback"]...)
	repo, err := s.store.RepoForJob(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "job not found")
		return
	}
	if err != nil {
		slog.Error("cache: resolve repo for job", "err", err, "job", id)
		writeErr(w, http.StatusInternalServerError, "failed to resolve job")
		return
	}
	blobKey, matched, err := s.store.LookupCache(r.Context(), repo, keys)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "cache miss")
		return
	}
	if err != nil {
		slog.Error("cache: lookup", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to look up cache")
		return
	}
	rc, err := s.blobs.Get(r.Context(), blobKey)
	if err != nil {
		// Metadata row exists but the blob is gone (e.g. mid-GC). Treat as a miss.
		slog.Warn("cache: blob missing for entry", "err", err, "key", blobKey)
		writeErr(w, http.StatusNotFound, "cache miss")
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("X-Cache-Key", matched)
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
	s.audit(r, "repo-settings.update", req.Repo, req.Repo, "ok", map[string]any{
		"default_runner_tags": req.DefaultRunnerTags,
	})
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
	s.audit(r, "member.add", m.Username, m.Repo, "ok", map[string]any{
		"username": m.Username,
		"role":     m.Role,
	})
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
	s.audit(r, "member.remove", strconv.FormatInt(id, 10), "", "ok", map[string]any{"id": id})
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
	s.audit(r, "protected-env.upsert", out.Name, out.Repo, "ok", map[string]any{
		"name":                out.Name,
		"required_approvals":  out.RequiredApprovals,
		"approver_roles":      out.ApproverRoles,
		"allow_self_approval": out.AllowSelfApproval,
	})
	writeJSON(w, http.StatusOK, out)
}
