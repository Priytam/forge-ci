package api

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/live"
	"github.com/priytamjeepandey/forge-ci/internal/proto"
	"github.com/priytamjeepandey/forge-ci/internal/store"
)

// Forge Live (batch 9). Agents POST batches; pages read. Every route that
// VIEWS live state is registered through liveReadRoutes and served with a
// read-path context from Postgres, the blob store and the hub's memory — the
// server holds no provider code (TestServerHasNoProviderCode), so a page can
// never reach a cluster or a cloud API.

// liveReadRoutes are all pages/APIs that show live state. The read-path test
// walks this table, so a new view route is covered by adding it here.
func (s *Server) liveReadRoutes() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"GET /api/v1/live/apps":                      s.listLiveApps,
		"GET /api/v1/live/apps/{id}":                 s.getLiveApp,
		"GET /api/v1/live/apps/{id}/events":          s.liveAppEvents,
		"GET /api/v1/live/apps/{id}/manifests/{uid}": s.liveManifest,
		"GET /api/v1/live/summary":                   s.liveSummary,
		"GET /api/v1/live/stream":                    s.streamLive,
		"GET /api/v1/live/targets":                   s.listLiveTargets,
		"GET /api/v1/live/apps/{id}/desired":         s.liveDesired,
		"GET /api/v1/live/apps/{id}/ops":             s.liveAppOps,
		"GET /api/v1/live/ops/{id}":                  s.getLiveOp,
		"GET /api/v1/live/ops/{id}/output":           s.streamLiveOpOutput,
		"GET /api/v1/live/apps/{id}/orphans":         s.liveOrphans,
	}
}

func (s *Server) registerLiveRoutes() {
	for pattern, h := range s.liveReadRoutes() {
		s.mux.HandleFunc(pattern, readPath(h))
	}
	s.mux.HandleFunc("POST /api/v1/live/targets", s.createLiveTarget)
	s.mux.HandleFunc("DELETE /api/v1/live/targets/{id}", s.deleteLiveTarget)
	s.mux.HandleFunc("POST /api/v1/live/agent/batches", s.liveAgentBatch)
	s.mux.HandleFunc("POST /api/v1/live/apps/{id}/ops", s.createLiveOp)
	s.mux.HandleFunc("PUT /api/v1/live/apps/{id}/policy", s.setLivePolicy)
	s.mux.HandleFunc("POST /api/v1/live/ops/{id}/approve", s.approveLiveOp)
	s.mux.HandleFunc("POST /api/v1/live/ops/{id}/cancel", s.cancelLiveOp)
	s.mux.HandleFunc("POST /api/v1/live/agent/ops/next", s.liveAgentNextOp)
	s.mux.HandleFunc("POST /api/v1/live/agent/ops/{id}/output", s.liveAgentOpOutput)
	s.mux.HandleFunc("POST /api/v1/live/agent/ops/{id}/result", s.liveAgentOpResult)
}

// RunLive runs Forge Live's background work (auto-sync, collecting
// unreferenced blobs) until ctx ends.
func (s *Server) RunLive(ctx context.Context) {
	go s.live.AutoSync(ctx)
	s.live.Sweep(ctx)
}

// maxLiveBatch caps a decompressed agent batch (agents send ≤2 MiB of
// resources per batch).
const maxLiveBatch = 64 << 20

// liveAgentBatch is POST /api/v1/live/agent/batches: one agent batch,
// authenticated by the target's agent token, answered with an Ack.
func (s *Server) liveAgentBatch(w http.ResponseWriter, r *http.Request) {
	t, ok := s.liveAgent(w, r)
	if !ok {
		return
	}
	counted := &countingReader{r: http.MaxBytesReader(w, r.Body, maxLiveBatch)}
	body := io.Reader(counted)
	if r.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(counted)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid gzip body")
			return
		}
		body = io.LimitReader(zr, maxLiveBatch)
	}
	var b live.Batch
	if err := json.NewDecoder(body).Decode(&b); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid batch: "+err.Error())
		return
	}
	if b.Epoch == "" || b.Seq < 1 {
		writeErr(w, http.StatusBadRequest, "epoch and seq are required")
		return
	}
	ack, err := s.live.Ingest(r.Context(), t.ID, b, counted.n)
	if err != nil {
		slog.Error("live: ingest", "target", t.Name, "seq", b.Seq, "err", err)
		writeErr(w, http.StatusInternalServerError, "ingest failed")
		return
	}
	writeJSON(w, http.StatusOK, ack)
}

// liveAgent authenticates an agent request by its target's token.
func (s *Server) liveAgent(w http.ResponseWriter, r *http.Request) (*store.LiveTarget, bool) {
	t, err := s.store.LiveTargetByToken(r.Context(), bearerToken(r))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusUnauthorized, "invalid agent token")
		return nil, false
	}
	if err != nil {
		slog.Error("live: resolve agent token", "err", err)
		writeErr(w, http.StatusInternalServerError, "token check failed")
		return nil, false
	}
	return t, true
}

type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// listLiveApps is the Applications page: GET /api/v1/live/apps with
// ?health= &sync= &environment= &repo= &target= &namespace= &group=
// (repeatable or comma-separated) &q= &limit= &offset=. It answers the
// page of apps, the total and the facet counts for the filter panel.
func (s *Server) listLiveApps(w http.ResponseWriter, r *http.Request) {
	q := store.LiveAppQuery{
		Health: queryList(r, "health"), Sync: queryList(r, "sync"), Environment: queryList(r, "environment"),
		Repo: queryList(r, "repo"), Target: queryList(r, "target"), Namespace: queryList(r, "namespace"),
		Group: queryList(r, "group"), Search: r.URL.Query().Get("q"), Limit: 500,
	}
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
		q.Limit = min(n, 2000)
	}
	if n, err := strconv.Atoi(r.URL.Query().Get("offset")); err == nil && n > 0 {
		q.Offset = n
	}
	apps, total, facets, err := s.store.ListLiveApps(r.Context(), q)
	if err != nil {
		slog.Error("live: list apps", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to list applications")
		return
	}
	writeJSONCached(w, r, map[string]any{"apps": apps, "total": total, "facets": facets})
}

// liveAppTarget is what an app page needs to know about where it runs.
type liveAppTarget struct {
	Name         string            `json:"name"`
	Provider     string            `json:"provider"`
	Capabilities live.Capabilities `json:"capabilities"`
	AgentVersion string            `json:"agent_version"`
	LastSeenAt   *time.Time        `json:"last_seen_at"`
	Synced       bool              `json:"synced"`
}

// getLiveApp is one application: its status row, where it runs and its
// resource tree. Pages then follow /live/stream?app= for patches.
func (s *Server) getLiveApp(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid application id")
		return
	}
	a, err := s.store.LiveApp(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "application not found")
		return
	}
	if err != nil {
		slog.Error("live: get app", "id", id, "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to load the application")
		return
	}
	t, err := s.store.LiveAppTarget(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to load the application's target")
		return
	}
	tree, err := s.live.Tree(r.Context(), a)
	if err != nil {
		slog.Error("live: load tree", "id", id, "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to load the resource tree")
		return
	}
	writeJSONCached(w, r, map[string]any{"app": a, "tree": tree, "target": liveAppTarget{
		Name: t.Name, Provider: t.Provider, Capabilities: t.Capabilities, AgentVersion: t.AgentVersion, LastSeenAt: t.LastSeenAt, Synced: t.Synced}})
}

// liveAppEvents is GET /api/v1/live/apps/{id}/events[?uid=resource uid].
func (s *Server) liveAppEvents(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid application id")
		return
	}
	events, err := s.store.LiveAppEvents(r.Context(), id, r.URL.Query().Get("uid"))
	if err != nil {
		slog.Error("live: events", "id", id, "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to load events")
		return
	}
	writeJSONCached(w, r, events)
}

// liveManifest is one resource's live manifest, as the agent last sent it.
func (s *Server) liveManifest(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid application id")
		return
	}
	b, err := s.live.Manifest(r.Context(), id, r.PathValue("uid"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "no manifest for this resource")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, no-cache")
	_, _ = w.Write(b)
}

// liveSummary is GET /api/v1/live/summary?repo=: environment → what runs
// there (the pipeline graph's Live nodes).
func (s *Server) liveSummary(w http.ResponseWriter, r *http.Request) {
	repo := r.URL.Query().Get("repo")
	if repo == "" {
		writeErr(w, http.StatusBadRequest, "repo is required")
		return
	}
	sums, err := s.store.LiveSummaries(r.Context(), []string{repo})
	if err != nil {
		slog.Error("live: summary", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to summarise")
		return
	}
	out := sums[repo]
	if out == nil {
		out = map[string]*proto.LiveSummary{}
	}
	writeJSONCached(w, r, out)
}

// liveSummaries is what runs per repo × environment, for the environment
// pages (nil, logged, when it cannot be read: those pages work without it).
func (s *Server) liveSummaries(r *http.Request, repos []string) map[string]map[string]*proto.LiveSummary {
	if len(repos) == 0 {
		return nil
	}
	sums, err := s.store.LiveSummaries(r.Context(), repos)
	if err != nil {
		slog.Warn("live: summaries for the environment pages", "err", err)
		return nil
	}
	return sums
}

// listLiveTargets lists clusters and projects with their agents' health.
func (s *Server) listLiveTargets(w http.ResponseWriter, r *http.Request) {
	ts, err := s.store.LiveTargets(r.Context())
	if err != nil {
		slog.Error("live: targets", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to list targets")
		return
	}
	writeJSON(w, http.StatusOK, ts)
}

// createLiveTarget registers a cluster or project and returns the agent
// token, once. Admin only.
func (s *Server) createLiveTarget(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var req struct {
		Name        string `json:"name"`
		Environment string `json:"environment"`
		Repo        string `json:"repo"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Name) == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}
	t, token, err := s.store.CreateLiveTarget(r.Context(), strings.TrimSpace(req.Name), strings.TrimSpace(req.Environment),
		strings.TrimSpace(req.Repo), s.actorLabel(r))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.audit(r, "live.target.create", t.Name, t.Repo, "success", map[string]any{"environment": t.Environment})
	writeJSON(w, http.StatusCreated, map[string]any{"target": t, "token": token})
}

// deleteLiveTarget removes a target (its token stops working at once).
func (s *Server) deleteLiveTarget(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid target id")
		return
	}
	if err := s.live.DeleteTarget(r.Context(), id); errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "target not found")
		return
	} else if err != nil {
		slog.Error("live: delete target", "id", id, "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to delete the target")
		return
	}
	s.audit(r, "live.target.delete", strconv.FormatInt(id, 10), "", "success", nil)
	w.WriteHeader(http.StatusNoContent)
}

// streamLive is GET /api/v1/live/stream[?app=ID]: "app" events carry an
// application's new summary row, "gone" its removal, and with ?app= "patch"
// events move that app's tree from one version to the next. A page that
// falls behind is disconnected; it reconnects and refetches on "ready".
func (s *Server) streamLive(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	app, _ := strconv.ParseInt(r.URL.Query().Get("app"), 10, 64)
	events, unsubscribe := s.live.Subscribe(r.Context(), app)
	defer unsubscribe()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "event: ready\ndata: {}\n\n")
	flusher.Flush()
	ping := time.NewTicker(sseKeepAlive)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case e, ok := <-events:
			if !ok {
				return
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Name, e.Data); err != nil {
				return
			}
			flusher.Flush()
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// ---- desired state and operations (phase B) ----

// liveDesired is GET /api/v1/live/apps/{id}/desired?key=: one desired
// manifest, as the agent rendered it from the source.
func (s *Server) liveDesired(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid application id")
		return
	}
	b, err := s.live.DesiredManifest(r.Context(), id, r.URL.Query().Get("key"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "no desired manifest for this resource")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, no-cache")
	_, _ = w.Write(b)
}

// liveAppOps is an application's history: its syncs, rollbacks and
// refreshes, newest first.
func (s *Server) liveAppOps(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid application id")
		return
	}
	ops, err := s.store.LiveAppOps(r.Context(), id, 100)
	if err != nil {
		slog.Error("live: ops", "id", id, "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to load the history")
		return
	}
	writeJSONCached(w, r, ops)
}

func (s *Server) getLiveOp(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid operation id")
		return
	}
	o, err := s.store.LiveOp(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "operation not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to load the operation")
		return
	}
	writeJSON(w, http.StatusOK, o)
}

// createLiveOp is POST /api/v1/live/apps/{id}/ops: sync, rollback, refresh
// or logs. It needs the operate role on the app's repo, a target that can
// do it (capabilities), and for a sync or rollback an environment that is
// not frozen; a protected environment's approvals come first.
func (s *Server) createLiveOp(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid application id")
		return
	}
	var op live.Op
	if err := json.NewDecoder(r.Body).Decode(&op); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	a, err := s.store.LiveApp(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "application not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to load the application")
		return
	}
	if !s.requireOperate(w, r, a.Repo) {
		return
	}
	t, err := s.store.LiveAppTarget(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to load the target")
		return
	}
	caps := t.Capabilities
	switch op.Kind {
	case live.OpSync:
		ok = caps.Sync
	case live.OpRollback:
		ok = caps.Rollback && op.Revision != ""
	case live.OpRefresh:
		ok = true
	case live.OpLogs:
		ok = caps.Logs && op.Pod != ""
	default:
		ok = false
	}
	if !ok {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("%s cannot %s this application", t.Name, op.Kind))
		return
	}
	// Providers whose desired state is the deploy record (Cloud Run) sync
	// to the image Forge last deployed.
	if op.Kind == live.OpSync && !caps.Diff && op.Image == "" {
		if op.Image = a.DeployedImage(); op.Image == "" {
			writeErr(w, http.StatusBadRequest, "no Forge deploy with an image digest to sync to")
			return
		}
	}
	// With auto-sync on, Git's revision is what runs: a sync to another
	// one would be undone at once. A rollback turns auto-sync off instead.
	offHead := op.Kind == live.OpRollback || op.Kind == live.OpSync && op.Revision != "" && op.Revision != a.Revision
	if offHead && a.Policy.AutoSync && !op.DryRun && op.Kind == live.OpSync {
		writeErr(w, http.StatusConflict, fmt.Sprintf("Auto-sync is on: it would sync %s straight back to %s. Roll back instead "+
			"(that turns auto-sync off), or turn auto-sync off first.", a.Name, shortRev(a.Revision)))
		return
	}
	o, err := s.store.CreateLiveOp(r.Context(), id, op, s.actorLabel(r))
	switch {
	case errors.Is(err, store.ErrFrozen), errors.Is(err, store.ErrOpBusy):
		writeErr(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		slog.Error("live: create op", "id", id, "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to start the operation")
		return
	}
	if op.Kind != live.OpLogs {
		s.audit(r, "live."+op.Kind, a.Target+"/"+a.Key, a.Repo, "requested", map[string]any{
			"op": o.ID, "revision": op.Revision, "prune": op.Prune, "dry_run": op.DryRun, "force": op.Force, "state": o.State})
	}
	if offHead && a.Policy.AutoSync && !op.DryRun {
		p := *a.Policy
		p.AutoSync, p.Status = false, "Turned off by the rollback to "+shortRev(op.Revision)
		if err := s.store.SetLivePolicy(r.Context(), id, p, s.actorLabel(r)); err != nil {
			slog.Error("live: turn auto-sync off", "id", id, "err", err)
		} else {
			s.audit(r, "live.policy", a.Target+"/"+a.Key, a.Repo, "updated", map[string]any{"auto_sync": false, "reason": "rollback", "op": o.ID})
		}
	}
	writeJSON(w, http.StatusCreated, o)
}

func shortRev(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

// setLivePolicy is PUT /api/v1/live/apps/{id}/policy: auto-sync, and with
// it prune and self-heal. It needs the operate role and a target that can
// sync; self-heal is refused where every change needs an approval (a heal
// that waits for a person is not one).
func (s *Server) setLivePolicy(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid application id")
		return
	}
	var p store.LivePolicy
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	a, err := s.store.LiveApp(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "application not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to load the application")
		return
	}
	if !s.requireOperate(w, r, a.Repo) {
		return
	}
	t, err := s.store.LiveAppTarget(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to load the target")
		return
	}
	if p.AutoSync && !t.Capabilities.Sync {
		writeErr(w, http.StatusBadRequest, t.Name+"'s agent cannot sync (it has no write access)")
		return
	}
	if p.AutoSync && p.SelfHeal {
		_, approvals, err := s.store.LiveChangeGate(r.Context(), a.Repo, a.Environment)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "failed to read the environment's rules")
			return
		}
		if approvals > 0 {
			writeErr(w, http.StatusConflict, fmt.Sprintf("Every change to %s needs an approval, so it cannot heal itself. "+
				"Auto-sync without self-heal works: each new revision waits for its approval.", a.Environment))
			return
		}
	}
	p.Status = ""
	if err := s.store.SetLivePolicy(r.Context(), id, p, s.actorLabel(r)); err != nil {
		slog.Error("live: set policy", "id", id, "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to save the sync policy")
		return
	}
	s.audit(r, "live.policy", a.Target+"/"+a.Key, a.Repo, "updated", map[string]any{
		"auto_sync": p.AutoSync, "prune": p.Prune, "self_heal": p.SelfHeal})
	if a, err = s.store.LiveApp(r.Context(), id); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to load the application")
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// liveOrphans is GET /api/v1/live/apps/{id}/orphans: the orphaned resources
// of the app's namespace (top-level objects no application claims), as a
// row and tree pages follow like an app's. Null when there are none.
func (s *Server) liveOrphans(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid application id")
		return
	}
	o, err := s.store.LiveOrphans(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusOK, nil)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to load orphaned resources")
		return
	}
	tree, err := s.live.Tree(r.Context(), o)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to load orphaned resources")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": o.ID, "tree": tree})
}

func (s *Server) approveLiveOp(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid operation id")
		return
	}
	var req struct {
		Verdict  string `json:"verdict"`
		Comment  string `json:"comment"`
		Approver string `json:"approver"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	// As for deploy jobs: with a session the approver is who is signed in.
	if sess := s.currentSession(r); sess != nil {
		req.Approver = sess.Email
	} else if s.ssoEnforced(r.Context()) {
		writeErr(w, http.StatusUnauthorized, "authentication required — sign in via SSO")
		return
	}
	if req.Approver == "" {
		req.Approver = s.actorLabel(r)
	}
	if req.Verdict != "approved" && req.Verdict != "rejected" {
		writeErr(w, http.StatusBadRequest, `verdict must be "approved" or "rejected"`)
		return
	}
	o, err := s.store.ApproveLiveOp(r.Context(), id, req.Approver, req.Verdict, req.Comment)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "operation not found")
	case errors.Is(err, store.ErrNotBlocked):
		writeErr(w, http.StatusConflict, "this operation is not waiting for approval")
	case errors.Is(err, store.ErrDuplicateVote):
		writeErr(w, http.StatusConflict, "you have already voted")
	case errors.Is(err, store.ErrForbidden), errors.Is(err, store.ErrSelfApproval):
		writeErr(w, http.StatusForbidden, err.Error())
	case err != nil:
		slog.Error("live: approve op", "id", id, "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to record the vote")
	default:
		s.audit(r, "live.approve", strconv.FormatInt(id, 10), "", req.Verdict, nil)
		writeJSON(w, http.StatusOK, o)
	}
}

func (s *Server) cancelLiveOp(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid operation id")
		return
	}
	o, err := s.store.LiveOp(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "operation not found")
		return
	}
	a, err := s.store.LiveApp(r.Context(), o.AppID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "application not found")
		return
	}
	if !s.requireOperate(w, r, a.Repo) {
		return
	}
	if err := s.store.CancelLiveOp(r.Context(), id, s.actorLabel(r)); err != nil && !errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusInternalServerError, "failed to cancel")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// streamLiveOpOutput is GET /api/v1/live/ops/{id}/output: an operation's
// output (pod logs) as server-sent "output" events, then "end". Leaving the
// page stops a log stream.
func (s *Server) streamLiveOpOutput(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid operation id")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	var last int64
	tick := time.NewTicker(300 * time.Millisecond)
	defer tick.Stop()
	for {
		o, err := s.store.LiveOp(r.Context(), id)
		if err != nil {
			return
		}
		chunks, next, err := s.store.LiveOpOutput(r.Context(), id, last)
		if err != nil {
			return
		}
		last = next
		for _, c := range chunks {
			data, _ := json.Marshal(string(c))
			if _, err := fmt.Fprintf(w, "event: output\ndata: %s\n\n", data); err != nil {
				return
			}
		}
		if o.FinishedAt != nil && len(chunks) == 0 {
			data, _ := json.Marshal(o)
			fmt.Fprintf(w, "event: end\ndata: %s\n\n", data)
			flusher.Flush()
			return
		}
		flusher.Flush()
		select {
		case <-r.Context().Done():
			if o.Kind == live.OpLogs {
				_ = s.store.CancelLiveOp(context.WithoutCancel(r.Context()), id, "the page")
			}
			return
		case <-tick.C:
		}
	}
}

// ---- agent side ----

// liveAgentNextOp is the agent's long poll: an operation (200) or none
// within 25 seconds (204).
func (s *Server) liveAgentNextOp(w http.ResponseWriter, r *http.Request) {
	t, ok := s.liveAgent(w, r)
	if !ok {
		return
	}
	o, err := s.live.NextOp(r.Context(), t.ID, 25*time.Second)
	if err != nil {
		slog.Error("live: next op", "target", t.Name, "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to read operations")
		return
	}
	if o == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, o.Params)
}

// liveAgentOpOutput stores a chunk of an operation's output; 409 tells the
// agent to stop (canceled, or over its budget).
func (s *Server) liveAgentOpOutput(w http.ResponseWriter, r *http.Request) {
	t, ok := s.liveAgent(w, r)
	if !ok {
		return
	}
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid operation id")
		return
	}
	chunk, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "unreadable body")
		return
	}
	kept, err := s.store.AppendLiveOpOutput(r.Context(), t.ID, id, chunk)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to store output")
		return
	}
	if !kept {
		writeErr(w, http.StatusConflict, "stop: the operation is over")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) liveAgentOpResult(w http.ResponseWriter, r *http.Request) {
	t, ok := s.liveAgent(w, r)
	if !ok {
		return
	}
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid operation id")
		return
	}
	var res live.OpResult
	if err := json.NewDecoder(r.Body).Decode(&res); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid result")
		return
	}
	o, err := s.store.FinishLiveOp(r.Context(), t.ID, id, res)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "no such running operation")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to record the result")
		return
	}
	if o.Kind != live.OpLogs {
		s.auditAs(r, "agent:"+t.Name, "live."+o.Kind+".finished", strconv.FormatInt(o.ID, 10), "", o.State, map[string]any{"message": o.Message})
	}
	w.WriteHeader(http.StatusNoContent)
}
