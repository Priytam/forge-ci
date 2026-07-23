package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

// ssePollInterval is the terminal-state check cadence, and the sole tick source
// for the postgres backend (which has no pub/sub and must poll for new bytes).
const ssePollInterval = time.Second

// streamLogs is the SSE live-tail endpoint:
//
//	GET /api/v1/jobs/{id}/logs/stream?offset=N   →   text/event-stream
//
// It replays the log from ?offset (default 0), then live-tails: with the redis
// backend it wakes on the job's pub/sub channel and pulls new bytes by offset;
// with the postgres backend it polls every second. Each frame is a "log" event
// carrying {bytes, next_offset, eof}; a final "eof" event is sent when the job
// reaches a terminal state and the reader has drained to the end. The handler
// exits on client disconnect (r.Context().Done()). Being a GET it is CSRF-exempt
// and, when SSO is enforced, still requires a session (handled upstream).
func (s *Server) streamLogs(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid job id")
		return
	}
	offset := int64(0)
	if v := r.URL.Query().Get("offset"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, "offset must be a non-negative integer")
			return
		}
		offset = n
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable proxy buffering (nginx)
	w.WriteHeader(http.StatusOK)

	s.logs.Metrics().SSEOpen()
	defer s.logs.Metrics().SSEClose()

	ctx := r.Context()

	// Live-tail wake source. Redis publishes per append; postgres has none, so
	// we fall back to polling on the ticker alone.
	var wake <-chan struct{}
	if sub, live := s.logs.Subscribe(ctx, id); live {
		defer sub.Close()
		wake = sub.C()
	}

	writeEvent := func(event string, payload any) error {
		b, _ := json.Marshal(payload)
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	// pump drains all bytes from the current offset, emitting one "log" event if
	// there are any, and returns whether the stream is complete (job terminal +
	// fully drained). Advances offset.
	pump := func() (done bool) {
		d, err := s.logs.ReadDelta(ctx, id, offset)
		if err != nil {
			slog.Error("sse read", "err", err, "job", id)
			return true
		}
		if len(d.Bytes) > 0 {
			if err := writeEvent("log", map[string]any{
				"bytes":       string(d.Bytes),
				"next_offset": d.NextOffset,
				"eof":         d.EOF,
			}); err != nil {
				return true // client gone
			}
			offset = d.NextOffset
		}
		if d.EOF {
			_ = writeEvent("eof", map[string]any{"next_offset": d.NextOffset, "eof": true})
			return true
		}
		return false
	}

	// Initial replay from offset.
	if pump() {
		return
	}

	ticker := time.NewTicker(ssePollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-wake:
			if pump() {
				return
			}
		case <-ticker.C:
			if pump() {
				return
			}
		}
	}
}
