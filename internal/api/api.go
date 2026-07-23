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
	"sync"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/blob"
	"github.com/priytamjeepandey/forge-ci/internal/compiler"
	"github.com/priytamjeepandey/forge-ci/internal/proto"
	"github.com/priytamjeepandey/forge-ci/internal/store"
)

const (
	acquireLongPoll = 25 * time.Second
	maxLogChunk     = 1 << 20 // 1 MiB per POST

	defaultMaxJobLogBytes  = 10 << 20  // MAX_JOB_LOG_BYTES  (10 MiB)
	defaultMaxArtifactByte = 500 << 20 // MAX_ARTIFACT_BYTES (500 MiB)

	defaultPageLimit = 50  // GET list endpoints default page size
	maxPageLimit     = 200 // hard cap on ?limit=
)

type Server struct {
	store *store.Store
	blobs blob.Store
	mux   *http.ServeMux
	sso   ssoCache

	runnerAuth       string // RUNNER_AUTH: "on" | "off"
	maxJobLogBytes   int64  // MAX_JOB_LOG_BYTES
	maxArtifactBytes int64  // MAX_ARTIFACT_BYTES

	// Per-job carry-over tail for chunk-boundary secret masking. Holds the
	// trailing raw bytes of the last log chunk that could still be the prefix
	// of a masked value completed by the next chunk.
	maskMu  sync.Mutex
	maskBuf map[int64]string
}

func New(s *store.Store, blobs blob.Store) *Server {
	srv := &Server{
		store:            s,
		blobs:            blobs,
		mux:              http.NewServeMux(),
		runnerAuth:       runnerAuthMode(),
		maxJobLogBytes:   envBytes("MAX_JOB_LOG_BYTES", defaultMaxJobLogBytes),
		maxArtifactBytes: envBytes("MAX_ARTIFACT_BYTES", defaultMaxArtifactByte),
		maskBuf:          map[int64]string{},
	}
	srv.initRunnerAuth()
	m := srv.mux

	// Public API.
	m.HandleFunc("POST /api/v1/pipelines", srv.createPipeline)
	m.HandleFunc("GET /api/v1/pipelines", srv.listPipelines)
	m.HandleFunc("GET /api/v1/pipelines/{id}", srv.getPipeline)
	m.HandleFunc("POST /api/v1/pipelines/{id}/cancel", srv.cancelPipeline)
	m.HandleFunc("GET /api/v1/repos", srv.listRepos)
	m.HandleFunc("GET /api/v1/stats", srv.dashboardStats)
	m.HandleFunc("GET /api/v1/jobs/{id}/logs", srv.getLogs)
	m.HandleFunc("POST /api/v1/jobs/{id}/approvals", srv.approve)
	m.HandleFunc("POST /api/v1/jobs/{id}/cancel", srv.cancelJob)
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
	srv.registerRunnerTokenRoutes()

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
	// CSRF: cookie-authenticated state-changing requests must be same-origin.
	if !s.checkCSRF(w, r) {
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
	opts, err := compiler.Options(req.Config)
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
	p, err := s.store.CreatePipeline(r.Context(), req, jobs, configVersion, opts.AutoCancel)
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

// listPipelines returns a page of pipelines as a JSON array (newest first).
// Pagination is controlled by ?limit (default 50, capped at 200) and ?offset.
// The total count and whether more pages exist are returned in the
// X-Total-Count and X-Has-More response headers so the array body stays
// compatible with existing clients.
func (s *Server) listPipelines(w http.ResponseWriter, r *http.Request) {
	limit, offset := pageParams(r)
	ps, total, err := s.store.ListPipelinesPage(r.Context(), r.URL.Query().Get("repo"), limit, offset)
	if err != nil {
		slog.Error("list pipelines", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to list pipelines")
		return
	}
	w.Header().Set("X-Total-Count", strconv.Itoa(total))
	w.Header().Set("X-Has-More", strconv.FormatBool(offset+len(ps) < total))
	writeJSON(w, http.StatusOK, ps)
}

// pageParams parses ?limit / ?offset with a default page size and a hard cap.
func pageParams(r *http.Request) (limit, offset int) {
	limit = defaultPageLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > maxPageLimit {
		limit = maxPageLimit
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			offset = n
		}
	}
	return limit, offset
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

// cancelPipeline cancels every non-terminal job in a pipeline. Idempotent: a
// finished pipeline is a clean no-op returning its current state.
func (s *Server) cancelPipeline(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid pipeline id")
		return
	}
	p, jobs, err := s.store.CancelPipeline(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "pipeline not found")
		return
	}
	if err != nil {
		slog.Error("cancel pipeline", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to cancel pipeline")
		return
	}
	slog.Info("pipeline cancel requested", "pipeline", id)
	writeJSON(w, http.StatusOK, map[string]any{"pipeline": p, "jobs": jobs})
}

// cancelJob cancels a single job by its current state. Idempotent for terminal
// jobs. A running job is signaled to stop via its next heartbeat.
func (s *Server) cancelJob(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid job id")
		return
	}
	job, err := s.store.CancelJob(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "job not found")
		return
	}
	if err != nil {
		slog.Error("cancel job", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to cancel job")
		return
	}
	slog.Info("job cancel requested", "job", id, "status", job.Status)
	writeJSON(w, http.StatusOK, map[string]any{"job": job})
}

// ---- runner handlers ----

// acquire long-polls for up to 25s waiting for a pending job; 204 when none.
func (s *Server) acquire(w http.ResponseWriter, r *http.Request) {
	if !s.requireRunnerAuth(w, r) {
		return
	}
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
	if !s.requireRunnerAuth(w, r) {
		return
	}
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
	// Redact masked variable values before the chunk is persisted. A secret can
	// be split across chunk boundaries, so we carry the trailing bytes that
	// could still be a partial match into the next chunk (see maskChunk). The
	// GetLogs read path re-masks as a final backstop.
	masked, _ := s.store.MaskedValuesForJob(r.Context(), id)
	chunk := s.maskChunk(id, string(body), masked)
	if chunk == "" {
		// Entire chunk was held back as a possible partial match; nothing to
		// store yet. It will flush with the next chunk or at completion.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if err := s.store.AppendLogCapped(r.Context(), id, chunk, s.maxJobLogBytes); err != nil {
		slog.Error("push logs", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to store logs")
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// maxMaskedLen returns the length of the longest masked value.
func maxMaskedLen(masked []string) int {
	n := 0
	for _, v := range masked {
		if len(v) > n {
			n = len(v)
		}
	}
	return n
}

// maskChunk masks masked values across chunk boundaries. It prepends the tail
// carried from the previous chunk, masks the combined text, then holds back a
// trailing window (up to maxLen-1 bytes) that could be the start of a masked
// value completed by the next chunk. The held-back window is chosen so that no
// masked value straddles the emitted/held boundary. Returns the text to store
// now (possibly empty when everything is still uncertain).
func (s *Server) maskChunk(jobID int64, chunk string, masked []string) string {
	maxLen := maxMaskedLen(masked)
	if maxLen == 0 {
		return chunk // nothing to mask
	}
	s.maskMu.Lock()
	combined := s.maskBuf[jobID] + chunk
	keep := maxLen - 1
	if len(combined) <= keep {
		s.maskBuf[jobID] = combined
		s.maskMu.Unlock()
		return ""
	}
	split := len(combined) - keep
	// Extend the split point rightward past any masked value that straddles it,
	// so the emitted prefix never cuts through a match. Iterate to a fixpoint
	// (an extension can pull in a further straddling match).
	for {
		moved := false
		for _, v := range masked {
			if v == "" {
				continue
			}
			from := split - len(v) + 1
			if from < 0 {
				from = 0
			}
			// A match straddles `split` if it starts before split and ends after.
			if idx := strings.Index(combined[from:], v); idx >= 0 {
				start := from + idx
				end := start + len(v)
				if start < split && end > split {
					split = end
					moved = true
				}
			}
		}
		if !moved || split >= len(combined) {
			break
		}
	}
	if split > len(combined) {
		split = len(combined)
	}
	emit := maskAll(combined[:split], masked)
	s.maskBuf[jobID] = combined[split:]
	s.maskMu.Unlock()
	return emit
}

// flushMaskTail masks and stores any bytes still held for a job, then drops its
// buffer. Called when the job leaves 'running' (complete/cancel/requeue) so no
// trailing output is lost.
func (s *Server) flushMaskTail(ctx context.Context, jobID int64) {
	s.maskMu.Lock()
	tail := s.maskBuf[jobID]
	delete(s.maskBuf, jobID)
	s.maskMu.Unlock()
	if tail == "" {
		return
	}
	masked, _ := s.store.MaskedValuesForJob(ctx, jobID)
	if err := s.store.AppendLogCapped(ctx, jobID, maskAll(tail, masked), s.maxJobLogBytes); err != nil {
		slog.Error("flush log tail", "err", err, "job", jobID)
	}
}

// maskAll replaces every masked value in text with [MASKED].
func maskAll(text string, masked []string) string {
	for _, v := range masked {
		if v != "" {
			text = strings.ReplaceAll(text, v, "[MASKED]")
		}
	}
	return text
}

// heartbeat stamps liveness and tells the runner whether the job has been
// marked for cancellation. The runner acts on {cancel:true} by killing the job
// and reporting completion as 'canceled'.
func (s *Server) heartbeat(w http.ResponseWriter, r *http.Request) {
	if !s.requireRunnerAuth(w, r) {
		return
	}
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid job id")
		return
	}
	cancel, err := s.store.Heartbeat(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "heartbeat failed")
		return
	}
	writeJSON(w, http.StatusOK, proto.HeartbeatResponse{Cancel: cancel})
}

func (s *Server) complete(w http.ResponseWriter, r *http.Request) {
	if !s.requireRunnerAuth(w, r) {
		return
	}
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
	switch req.Status {
	case "success", "failed", "canceled", "requeue":
	default:
		writeErr(w, http.StatusBadRequest, `status must be "success", "failed", "canceled" or "requeue"`)
		return
	}
	// Flush any carried-over log tail for this job before it leaves 'running'.
	s.flushMaskTail(r.Context(), id)
	final, err := s.store.CompleteJob(r.Context(), id, req.Status, req.ExitCode)
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
	slog.Info("job completed", "job", id, "reported", req.Status, "final", final, "exit_code", req.ExitCode)
	w.WriteHeader(http.StatusOK)
}
