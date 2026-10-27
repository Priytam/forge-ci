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
	}
}

func (s *Server) registerLiveRoutes() {
	for pattern, h := range s.liveReadRoutes() {
		s.mux.HandleFunc(pattern, readPath(h))
	}
	s.mux.HandleFunc("POST /api/v1/live/targets", s.createLiveTarget)
	s.mux.HandleFunc("DELETE /api/v1/live/targets/{id}", s.deleteLiveTarget)
	s.mux.HandleFunc("POST /api/v1/live/agent/batches", s.liveAgentBatch)
}

// RunLive runs Forge Live's background work (collecting unreferenced blobs)
// until ctx ends.
func (s *Server) RunLive(ctx context.Context) { s.live.Sweep(ctx) }

// maxLiveBatch caps a decompressed agent batch (agents send ≤2 MiB of
// resources per batch).
const maxLiveBatch = 64 << 20

// liveAgentBatch is POST /api/v1/live/agent/batches: one agent batch,
// authenticated by the target's agent token, answered with an Ack.
func (s *Server) liveAgentBatch(w http.ResponseWriter, r *http.Request) {
	t, err := s.store.LiveTargetByToken(r.Context(), bearerToken(r))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusUnauthorized, "invalid agent token")
		return
	}
	if err != nil {
		slog.Error("live: resolve agent token", "err", err)
		writeErr(w, http.StatusInternalServerError, "token check failed")
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
