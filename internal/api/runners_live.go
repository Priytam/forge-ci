package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

// Runners page (batch 8): live updates over SSE, and the events (runner
// operations, maintenance, Forge deploys) its charts and drawer mark.

// runnerStreamInterval is how often the stream re-reads runners and load;
// a frame goes out only when something changed.
const runnerStreamInterval = 3 * time.Second

func (s *Server) registerRunnerLiveRoutes() {
	m := s.mux
	m.HandleFunc("GET /api/v1/runners/stream", s.streamRunners)
	m.HandleFunc("GET /api/v1/runner-events", s.runnerEvents)
}

// runnersView lists runners with Outdated set against the bundled runner.
func (s *Server) runnersView(ctx context.Context) ([]proto.Runner, error) {
	runners, err := s.store.ListRunners(ctx)
	if err != nil {
		return nil, err
	}
	bundled := bundledRunnerVersion()
	for i := range runners {
		v := runners[i].Version
		runners[i].Outdated = bundled != "" && bundled != "dev" && v != "" && v != "dev" && v != bundled
	}
	return runners, nil
}

// RunnerEvent is one mark on the runners page's time axis.
type RunnerEvent struct {
	At     time.Time      `json:"at"`
	Action string         `json:"action"` // runner.pause, runner.drain, maintenance.set, server.upgrade, …
	Target string         `json:"target"` // runner id, or the version deployed
	Actor  string         `json:"actor,omitempty"`
	Detail map[string]any `json:"detail,omitempty"`
}

// runnerEvents: ?range= (as the history) &runner= — runner operations,
// maintenance windows and Forge deploys in the range, oldest first. Anyone
// who can see the runners page sees the events; who did it is admin-only,
// as the audit log is.
func (s *Server) runnerEvents(w http.ResponseWriter, r *http.Request) {
	rg, err := parseRangeQuery(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	entries, err := s.store.ListEvents(r.Context(), rg.From, rg.To, r.URL.Query().Get("runner"), 500)
	if err != nil {
		slog.Error("runner events", "err", err)
		writeErr(w, http.StatusInternalServerError, "failed to load events")
		return
	}
	admin := s.isAdmin(r)
	out := make([]RunnerEvent, 0, len(entries))
	for _, e := range entries {
		ev := RunnerEvent{At: e.TS, Action: e.Action, Target: e.Target, Detail: e.Detail}
		if admin {
			ev.Actor = e.Actor
		}
		out = append(out, ev)
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": out})
}

// streamRunners is the runners page's live feed:
//
//	GET /api/v1/runners/stream?range=24h   →   text/event-stream
//
// A "snapshot" event carries {runners, load} (load over ?range, as
// /runner-load) on connect and whenever either changes; a comment line keeps
// idle connections open. It ends when the client disconnects.
func (s *Server) streamRunners(w http.ResponseWriter, r *http.Request) {
	if _, err := parseRangeQuery(r); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	ctx := r.Context()
	var last []byte
	idle := 0
	push := func() error {
		// Relative ranges slide with the clock, so re-read the range each time.
		rg, _ := parseRangeQuery(r)
		runners, err := s.runnersView(ctx)
		if err != nil {
			return err
		}
		load, err := s.store.GetRunnerLoad(ctx, rg.From, rg.To)
		if err != nil {
			return err
		}
		b, _ := json.Marshal(map[string]any{"runners": runners, "load": load})
		if bytes.Equal(b, last) {
			// Nothing changed: a comment every ~30s keeps proxies from
			// closing the connection.
			if idle++; idle%10 == 0 {
				_, err := fmt.Fprint(w, ": idle\n\n")
				flusher.Flush()
				return err
			}
			return nil
		}
		last, idle = b, 0
		if _, err := fmt.Fprintf(w, "event: snapshot\ndata: %s\n\n", b); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}
	if err := push(); err != nil {
		slog.Warn("runner stream", "err", err)
		return
	}
	tick := time.NewTicker(runnerStreamInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := push(); err != nil {
				if ctx.Err() == nil {
					slog.Warn("runner stream", "err", err)
				}
				return
			}
		}
	}
}
