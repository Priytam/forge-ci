// Package logstore is the high-volume log tier. It separates a job's live log
// (written continuously while the job runs, read for the live tail) from its
// archived log (one immutable object per finished job in the blob store).
//
// Two swappable live-buffer backends implement the same Backend interface:
//
//   - redis:    the production backend. Each running job's log is a capped Redis
//     String appended by APPEND and read by byte offset with GETRANGE, plus a
//     per-job pub/sub channel for live fan-out (SSE). On terminal transition the
//     buffer is flushed to the blob store as one object and evicted (short TTL).
//     Postgres never accumulates log bodies — only a pointer to the object.
//   - postgres: the local-dev / no-Redis fallback. Log bodies live in the
//     job_logs table exactly as before; reads re-assemble with string_agg and a
//     read-path masking backstop. SSE degrades to a poll-and-push loop.
//
// Backend resolution (see Resolve): LOG_BACKEND=redis|postgres wins when set; a
// LOG_BACKEND=redis that cannot reach Redis falls back to postgres with a loud
// warning rather than crashing. When LOG_BACKEND is unset, Redis is used iff
// REDIS_URL is set and reachable, else postgres.
//
// Masking is done by the API layer BEFORE bytes reach this package (chunk-
// boundary carry-over lives there); a read-path backstop is applied to full-text
// reads here as a second line of defence, matching the legacy behaviour.
package logstore

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/blob"
	"github.com/priytamjeepandey/forge-ci/internal/store"
)

// evictTTL is how long a finished job's Redis buffer lingers after it has been
// archived to the blob store, so an in-flight reader mid-transition is not cut
// off. Reads prefer the archived object once the pointer is set.
const evictTTL = 60 * time.Second

// runningTTL bounds the lifetime of a live buffer so a job whose runner crashes
// without ever reporting completion cannot leak a Redis key forever. Refreshed
// on every append.
const runningTTL = 24 * time.Hour

// archiveScanWindow / archiveScanLimit bound the scheduler's safety-net sweep
// for terminal-but-unarchived jobs (those that died without a runner complete
// call — stale/overdue/canceled-while-pending).
const (
	archiveScanWindow = time.Hour
	archiveScanLimit  = 200
)

// AppendResult reports the outcome of a live-buffer append.
type AppendResult struct {
	Total        int64 // cumulative buffered bytes after this append
	TruncatedNow bool  // the cap fired on THIS call (for metrics)
}

// Subscription is a live-tail notification stream. Each value delivered is a
// wake signal ("new bytes are available"); the SSE reader then pulls the actual
// delta by offset, so a dropped/late notification never corrupts the stream.
type Subscription interface {
	C() <-chan struct{}
	Close()
}

// Backend is a swappable live-buffer implementation (redis or postgres).
type Backend interface {
	Kind() string
	// Append writes already-masked bytes to the job's live buffer, enforcing a
	// cumulative cap (capBytes<=0 disables). It emits the truncation notice once.
	Append(ctx context.Context, jobID int64, p []byte, capBytes int64) (AppendResult, error)
	// ReadFrom returns live-buffer bytes from offset to the end, plus the total
	// buffered length. offset is clamped into range.
	ReadFrom(ctx context.Context, jobID int64, offset int64) (data []byte, total int64, err error)
	// Snapshot returns the entire live buffer (used to archive it).
	Snapshot(ctx context.Context, jobID int64) ([]byte, error)
	// Truncated reports whether the cap fired for this job.
	Truncated(ctx context.Context, jobID int64) (bool, error)
	// Evict schedules removal of the live buffer after ttl (<=0 removes now).
	Evict(ctx context.Context, jobID int64, ttl time.Duration) error
	// Subscribe returns a wake stream for the job; ok=false when the backend has
	// no native pub/sub and the caller must poll instead.
	Subscribe(ctx context.Context, jobID int64) (sub Subscription, ok bool)
	Close() error
}

// Service composes a live-buffer Backend with the blob store (archive) and the
// Postgres store (job status, archive pointers, masked-value backstop). It is
// what the API and scheduler talk to.
type Service struct {
	backend Backend
	blobs   blob.Store
	store   *store.Store
	metrics *Metrics
}

func isTerminal(status string) bool {
	return status == "success" || status == "failed" || status == "canceled"
}

// Resolve selects and constructs the log backend from the environment.
func Resolve(ctx context.Context, st *store.Store, blobs blob.Store) *Service {
	m := NewMetrics()
	want := strings.ToLower(strings.TrimSpace(os.Getenv("LOG_BACKEND")))
	redisURL := strings.TrimSpace(os.Getenv("REDIS_URL"))
	if want == "" {
		if redisURL != "" {
			want = "redis"
		} else {
			want = "postgres"
		}
	}
	svc := &Service{blobs: blobs, store: st, metrics: m}
	if want == "redis" {
		rb, err := newRedisBackend(ctx, redisURL)
		if err != nil {
			m.RedisUnavailable.Add(1)
			slog.Warn("log backend: LOG_BACKEND=redis but Redis is unavailable — "+
				"falling back to the postgres log backend (logs will live in Postgres, "+
				"SSE degrades to polling). Fix REDIS_URL to enable the high-volume tier.",
				"redis_url", redactURL(redisURL), "err", err)
			svc.backend = newPGBackend(st)
			return svc
		}
		slog.Info("log backend: redis (live buffer + blob archive)", "redis_url", redactURL(redisURL),
			"archive_store", blobs.Kind())
		svc.backend = rb
		return svc
	}
	slog.Info("log backend: postgres (log bodies in job_logs; dev/no-Redis path)")
	svc.backend = newPGBackend(st)
	return svc
}

// Backend reports the active backend kind ("redis" | "postgres").
func (s *Service) Backend() string { return s.backend.Kind() }

// Metrics returns the shared metrics registry.
func (s *Service) Metrics() *Metrics { return s.metrics }

// Close releases backend resources.
func (s *Service) Close() error { return s.backend.Close() }

// Append writes already-masked bytes to the job's live buffer and records
// ingest metrics. A backend error in redis mode is counted as a Redis-
// unavailability event; the caller decides how to respond to the runner.
func (s *Service) Append(ctx context.Context, jobID int64, p []byte, capBytes int64) (AppendResult, error) {
	if len(p) == 0 {
		return AppendResult{}, nil
	}
	res, err := s.backend.Append(ctx, jobID, p, capBytes)
	if err != nil {
		if s.backend.Kind() == "redis" {
			s.metrics.RedisUnavailable.Add(1)
		}
		return res, err
	}
	s.metrics.BytesIngested.Add(int64(len(p)))
	if res.TruncatedNow {
		s.metrics.Truncations.Add(1)
	}
	return res, nil
}

// maskBackstop re-masks a fully-assembled log body, matching the legacy
// GetLogs behaviour. Applied only to full-text reads, never to incremental
// slices (which could cut through a value); ingest-time masking with chunk-
// boundary carry-over already covers the incremental path.
func (s *Service) maskBackstop(ctx context.Context, jobID int64, body string) string {
	masked, err := s.store.MaskedValuesForJob(ctx, jobID)
	if err != nil {
		return body
	}
	for _, v := range masked {
		if v != "" {
			body = strings.ReplaceAll(body, v, "[MASKED]")
		}
	}
	return body
}

func (s *Service) readBlob(ctx context.Context, key string) ([]byte, error) {
	rc, err := s.blobs.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// FullText returns a job's entire log as text/plain (back-compat path), with
// the masking backstop applied. Running jobs read from the live buffer; finished
// jobs in redis mode read from the archived blob object; postgres mode always
// reads job_logs.
func (s *Service) FullText(ctx context.Context, jobID int64) (string, error) {
	if s.backend.Kind() != "redis" {
		// Postgres path: unchanged legacy behaviour (string_agg + backstop).
		return s.store.GetLogs(ctx, jobID)
	}
	meta, err := s.store.GetJobLogMeta(ctx, jobID)
	if err != nil {
		return "", err
	}
	if isTerminal(meta.Status) && meta.ObjectKey != nil {
		if *meta.ObjectKey == "" {
			return "", nil // archived, but the job produced no output
		}
		data, err := s.readBlob(ctx, *meta.ObjectKey)
		if err != nil {
			return "", err
		}
		return s.maskBackstop(ctx, jobID, string(data)), nil
	}
	data, _, err := s.backend.ReadFrom(ctx, jobID, 0)
	if err != nil {
		return "", err
	}
	return s.maskBackstop(ctx, jobID, string(data)), nil
}

// Delta is the incremental read result (GET .../logs?offset=N).
type Delta struct {
	Bytes      []byte
	NextOffset int64
	EOF        bool
}

// ReadDelta returns the log bytes from offset to the current end, the next
// offset to poll from, and whether the log is complete (job terminal and offset
// reached the end). Honors both backends and archived objects.
func (s *Service) ReadDelta(ctx context.Context, jobID int64, offset int64) (Delta, error) {
	if offset < 0 {
		offset = 0
	}
	meta, err := s.store.GetJobLogMeta(ctx, jobID)
	if err != nil {
		return Delta{}, err
	}
	terminal := isTerminal(meta.Status)

	// Archived object (redis mode, finished, pointer set) or postgres mode both
	// resolve to a full body we slice; running redis reads by offset (efficient).
	if s.backend.Kind() == "redis" && terminal && meta.ObjectKey != nil {
		var full []byte
		if *meta.ObjectKey != "" {
			if full, err = s.readBlob(ctx, *meta.ObjectKey); err != nil {
				return Delta{}, err
			}
		}
		return sliceDelta(full, offset, true), nil
	}
	if s.backend.Kind() != "redis" {
		full, err := s.store.GetLogs(ctx, jobID) // masked
		if err != nil {
			return Delta{}, err
		}
		return sliceDelta([]byte(full), offset, terminal), nil
	}
	// Redis live buffer (running, or terminal but not yet archived).
	data, total, err := s.backend.ReadFrom(ctx, jobID, offset)
	if err != nil {
		return Delta{}, err
	}
	next := offset + int64(len(data))
	return Delta{Bytes: data, NextOffset: next, EOF: terminal && next >= total}, nil
}

func sliceDelta(full []byte, offset int64, terminal bool) Delta {
	total := int64(len(full))
	if offset > total {
		offset = total
	}
	data := full[offset:]
	next := offset + int64(len(data))
	return Delta{Bytes: data, NextOffset: next, EOF: terminal && next >= total}
}

// Subscribe returns a live-tail wake stream for a job; ok=false means the
// backend has no pub/sub (poll instead).
func (s *Service) Subscribe(ctx context.Context, jobID int64) (Subscription, bool) {
	return s.backend.Subscribe(ctx, jobID)
}

// JobStatus returns the job's current status (used by SSE to detect terminal).
func (s *Service) JobStatus(ctx context.Context, jobID int64) (string, error) {
	meta, err := s.store.GetJobLogMeta(ctx, jobID)
	if err != nil {
		return "", err
	}
	return meta.Status, nil
}

// Archive flushes a finished job's live buffer to the blob store as one object,
// records the pointer, and evicts the buffer (short TTL). No-op for the postgres
// backend (bodies already live in Postgres) and idempotent (a job whose pointer
// is already set is skipped). Safe to call for a job that produced no output
// (records an empty-key pointer so the sweep won't retry it).
func (s *Service) Archive(ctx context.Context, jobID int64) error {
	if s.backend.Kind() != "redis" {
		return nil
	}
	meta, err := s.store.GetJobLogMeta(ctx, jobID)
	if err != nil {
		return err
	}
	if meta.ObjectKey != nil {
		return nil // already archived
	}
	if !isTerminal(meta.Status) {
		return nil // not terminal yet
	}
	start := time.Now()
	data, err := s.backend.Snapshot(ctx, jobID)
	if err != nil {
		s.metrics.ArchiveErrors.Add(1)
		return err
	}
	truncated, _ := s.backend.Truncated(ctx, jobID)
	key := ""
	if len(data) > 0 {
		key = fmt.Sprintf("logs/job-%d.log", jobID)
		if _, err := s.blobs.Put(ctx, key, bytes.NewReader(data)); err != nil {
			s.metrics.ArchiveErrors.Add(1)
			return err
		}
	}
	if err := s.store.SetLogPointer(ctx, jobID, key, int64(len(data)), truncated); err != nil {
		s.metrics.ArchiveErrors.Add(1)
		return err
	}
	// Keep the buffer briefly so a reader that started before the pointer flip
	// still finds it; reads prefer the object once the pointer is set.
	_ = s.backend.Evict(ctx, jobID, evictTTL)
	s.metrics.ArchiveOK.Add(1)
	s.metrics.ArchiveMillis.Add(time.Since(start).Milliseconds())
	slog.Info("log archived", "job", jobID, "key", key, "bytes", len(data), "truncated", truncated)
	return nil
}

// ArchivePending is the scheduler safety net: it archives terminal jobs that
// were never flushed by the runner-complete path (stale/overdue/canceled). It is
// a no-op for the postgres backend. Returns how many jobs it archived.
func (s *Service) ArchivePending(ctx context.Context) (int, error) {
	if s.backend.Kind() != "redis" {
		return 0, nil
	}
	ids, err := s.store.TerminalUnarchivedJobs(ctx, archiveScanWindow, archiveScanLimit)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		if err := s.Archive(ctx, id); err != nil {
			slog.Error("archive pending log", "job", id, "err", err)
			continue
		}
		n++
	}
	return n, nil
}

// redactURL hides any password embedded in a redis:// URL for logging.
func redactURL(u string) string {
	if u == "" {
		return "(empty)"
	}
	if at := strings.LastIndex(u, "@"); at >= 0 {
		if slash := strings.Index(u, "//"); slash >= 0 && slash+2 < at {
			return u[:slash+2] + "***@" + u[at+1:]
		}
	}
	return u
}
