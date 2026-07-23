// Package api exposes the public REST API (UI/CLI) and the runner protocol.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/blob"
	"github.com/priytamjeepandey/forge-ci/internal/compiler"
	"github.com/priytamjeepandey/forge-ci/internal/proto"
	"github.com/priytamjeepandey/forge-ci/internal/store"
)

const (
	acquireLongPoll = 25 * time.Second
	maxLogChunk     = 1 << 20 // 1 MiB per POST
)

type Server struct {
	store *store.Store
	blobs blob.Store
	mux   *http.ServeMux
	sso   ssoCache
}

func New(s *store.Store, blobs blob.Store) *Server {
	srv := &Server{store: s, blobs: blobs, mux: http.NewServeMux()}
	m := srv.mux

	// Public API.
	m.HandleFunc("POST /api/v1/pipelines", srv.createPipeline)
	m.HandleFunc("GET /api/v1/pipelines", srv.listPipelines)
	m.HandleFunc("GET /api/v1/pipelines/{id}", srv.getPipeline)
	m.HandleFunc("GET /api/v1/repos", srv.listRepos)
	m.HandleFunc("GET /api/v1/stats", srv.dashboardStats)
	m.HandleFunc("GET /api/v1/jobs/{id}/logs", srv.getLogs)
	m.HandleFunc("POST /api/v1/jobs/{id}/approvals", srv.approve)
	m.HandleFunc("GET /api/v1/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Runner protocol.
	m.HandleFunc("POST /api/v1/runner/acquire", srv.acquire)
	m.HandleFunc("POST /api/v1/runner/jobs/{id}/logs", srv.pushLogs)
	m.HandleFunc("POST /api/v1/runner/jobs/{id}/heartbeat", srv.heartbeat)
	m.HandleFunc("POST /api/v1/runner/jobs/{id}/complete", srv.complete)

	srv.registerSettingsRoutes()
	srv.registerWebhookRoutes()
	srv.registerAuthRoutes()

	return srv
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Permissive CORS: fine for local dev; put real authn/authz here later.
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// Session enforcement: active once any SSO provider is enabled.
	if !s.requireAuth(w, r) {
		return
	}
	s.mux.ServeHTTP(w, r)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

// ---- public handlers ----

func (s *Server) createPipeline(w http.ResponseWriter, r *http.Request) {
	var req proto.CreatePipelineRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Repo == "" || req.Ref == "" || req.Config == "" {
		writeErr(w, http.StatusBadRequest, "repo, ref and config are required")
		return
	}
	// Connected repos resolve the ref tip themselves — an explicit sha is an
	// advanced override. Unconnected repos have nothing to resolve against.
	if req.SHA == "" {
		sha, err := s.store.ResolveRef(r.Context(), req.Repo, req.Ref)
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusBadRequest,
				"sha is required for repos that are not connected (Repos → Add repository)")
			return
		}
		if err != nil {
			writeErr(w, http.StatusBadRequest, "could not resolve ref: "+err.Error())
			return
		}
		req.SHA = sha
	}
	jobs, err := compiler.Compile(req.Config, req.Ref)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// Stamp the config version when the submitted YAML matches the current
	// registered config; otherwise it's recorded as a one-off custom run.
	var configVersion *int
	if current, err := s.store.GetRepoConfig(r.Context(), req.Repo); err == nil && current == req.Config {
		if v, err := s.store.CurrentConfigVersion(r.Context(), req.Repo); err == nil && v > 0 {
			configVersion = &v
		}
	}
	p, err := s.store.CreatePipeline(r.Context(), req, jobs, configVersion)
	if err != nil {
		slog.Error("create pipeline", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to create pipeline")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"pipeline": p})
}

func (s *Server) listRepos(w http.ResponseWriter, r *http.Request) {
	repos, err := s.store.ListRepos(r.Context())
	if err != nil {
		slog.Error("list repos", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to list repos")
		return
	}
	writeJSON(w, http.StatusOK, repos)
}

func (s *Server) dashboardStats(w http.ResponseWriter, r *http.Request) {
	stats, err := s.store.DashboardStats(r.Context())
	if err != nil {
		slog.Error("stats", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to compute stats")
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

func (s *Server) listPipelines(w http.ResponseWriter, r *http.Request) {
	ps, err := s.store.ListPipelines(r.Context(), r.URL.Query().Get("repo"))
	if err != nil {
		slog.Error("list pipelines", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to list pipelines")
		return
	}
	writeJSON(w, http.StatusOK, ps)
}

func (s *Server) getPipeline(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid pipeline id")
		return
	}
	p, jobs, err := s.store.GetPipeline(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "pipeline not found")
		return
	}
	if err != nil {
		slog.Error("get pipeline", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to load pipeline")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pipeline": p, "jobs": jobs})
}

func (s *Server) getLogs(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid job id")
		return
	}
	logs, err := s.store.GetLogs(r.Context(), id)
	if err != nil {
		slog.Error("get logs", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to load logs")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, logs)
}

func (s *Server) approve(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid job id")
		return
	}
	var req proto.ApprovalRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	// With a live SSO session the approver is the AUTHENTICATED identity —
	// the client-supplied name is ignored.
	if sess := s.currentSession(r); sess != nil {
		req.Approver = sess.Email
	}
	if req.Approver == "" {
		writeErr(w, http.StatusBadRequest, "approver is required")
		return
	}
	if req.Verdict != "approved" && req.Verdict != "rejected" {
		writeErr(w, http.StatusBadRequest, `verdict must be "approved" or "rejected"`)
		return
	}
	job, err := s.store.Approve(r.Context(), id, req)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "job not found")
	case errors.Is(err, store.ErrNotBlocked):
		writeErr(w, http.StatusConflict, "job is not waiting for approval")
	case errors.Is(err, store.ErrDuplicateVote):
		writeErr(w, http.StatusConflict, "you have already voted on this job")
	case errors.Is(err, store.ErrForbidden):
		writeErr(w, http.StatusForbidden, err.Error())
	case errors.Is(err, store.ErrSelfApproval):
		writeErr(w, http.StatusForbidden, err.Error())
	case err != nil:
		slog.Error("approve", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to record approval")
	default:
		writeJSON(w, http.StatusOK, map[string]any{"job": job})
	}
}

// ---- runner handlers ----

// acquire long-polls for up to 25s waiting for a pending job; 204 when none.
func (s *Server) acquire(w http.ResponseWriter, r *http.Request) {
	var req proto.AcquireRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.RunnerID == "" {
		writeErr(w, http.StatusBadRequest, "runner_id is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), acquireLongPoll)
	defer cancel()
	for {
		job, err := s.store.AcquireJob(ctx, req)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			slog.Error("acquire", "err", err)
			writeErr(w, http.StatusInternalServerError, "queue error")
			return
		}
		if job != nil {
			slog.Info("job acquired", "job", job.ID, "runner", req.RunnerID)
			writeJSON(w, http.StatusOK, job)
			return
		}
		select {
		case <-ctx.Done():
			w.WriteHeader(http.StatusNoContent)
			return
		case <-time.After(time.Second):
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) pushLogs(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid job id")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxLogChunk))
	if err != nil || len(body) == 0 {
		writeErr(w, http.StatusBadRequest, "empty log chunk")
		return
	}
	// Redact masked variable values before the chunk is persisted. (Values
	// split across chunk boundaries can escape this — documented limitation.)
	chunk := string(body)
	if masked, err := s.store.MaskedValuesForJob(r.Context(), id); err == nil {
		for _, v := range masked {
			chunk = strings.ReplaceAll(chunk, v, "[MASKED]")
		}
	}
	if err := s.store.AppendLog(r.Context(), id, chunk); err != nil {
		slog.Error("push logs", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to store logs")
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) heartbeat(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid job id")
		return
	}
	if err := s.store.Heartbeat(r.Context(), id); err != nil {
		writeErr(w, http.StatusInternalServerError, "heartbeat failed")
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) complete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid job id")
		return
	}
	var req proto.CompleteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Status != "success" && req.Status != "failed" {
		writeErr(w, http.StatusBadRequest, `status must be "success" or "failed"`)
		return
	}
	err := s.store.CompleteJob(r.Context(), id, req.Status, req.ExitCode)
	if errors.Is(err, store.ErrNotFound) {
		// Job already transitioned (e.g. failed as stale) — not the runner's problem.
		w.WriteHeader(http.StatusConflict)
		return
	}
	if err != nil {
		slog.Error("complete", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to complete job")
		return
	}
	slog.Info("job completed", "job", id, "status", req.Status, "exit_code", req.ExitCode)
	w.WriteHeader(http.StatusOK)
}
