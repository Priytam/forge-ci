// Package scheduler drives the job state machine. Every transition it applies
// is an idempotent SQL statement, so running it after a crash — or running two
// schedulers concurrently — cannot corrupt state.
package scheduler

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/blob"
	"github.com/priytamjeepandey/forge-ci/internal/store"
)

type Scheduler struct {
	store      *store.Store
	blobs      blob.Store
	tick       time.Duration
	staleAfter time.Duration
	gcEvery    time.Duration // retention GC cadence
	retention  time.Duration // RETENTION_DAYS as a duration; 0 = keep forever
}

func New(s *store.Store, blobs blob.Store) *Scheduler {
	return &Scheduler{
		store:      s,
		blobs:      blobs,
		tick:       time.Second,
		staleAfter: 90 * time.Second,
		gcEvery:    time.Hour,
		retention:  retentionWindow(),
	}
}

// retentionWindow reads RETENTION_DAYS (default 30). 0 disables pipeline/
// artifact GC (expired sessions are always collected).
func retentionWindow() time.Duration {
	days := 30
	if v := os.Getenv("RETENTION_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			days = n
		}
	}
	return time.Duration(days) * 24 * time.Hour
}

func (sc *Scheduler) Run(ctx context.Context) {
	t := time.NewTicker(sc.tick)
	defer t.Stop()
	gc := time.NewTicker(sc.gcEvery)
	defer gc.Stop()
	// Run one GC pass shortly after startup so operators see it work without
	// waiting a full hour.
	firstGC := time.After(30 * time.Second)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sc.step(ctx)
		case <-firstGC:
			sc.gc(ctx)
		case <-gc.C:
			sc.gc(ctx)
		}
	}
}

// gc runs retention cleanup: expired sessions always, and pipelines (with
// their artifact blobs) older than the retention window when one is set.
func (sc *Scheduler) gc(ctx context.Context) {
	if n, err := sc.store.DeleteExpiredSessions(ctx); err != nil {
		slog.Error("gc: delete expired sessions", "err", err)
	} else if n > 0 {
		slog.Info("gc: deleted expired sessions", "rows", n)
	}

	if sc.retention <= 0 {
		return // retention disabled — keep pipelines/artifacts forever
	}

	// Delete artifact blobs first (we still have the DB rows to find them),
	// then cascade-delete the pipeline rows.
	keys, err := sc.store.ExpiredArtifactBlobKeys(ctx, sc.retention)
	if err != nil {
		slog.Error("gc: list expired artifact blobs", "err", err)
		return
	}
	blobsDeleted := 0
	for _, k := range keys {
		if err := sc.blobs.Delete(ctx, k); err != nil {
			slog.Error("gc: delete artifact blob", "err", err, "key", k)
			continue
		}
		blobsDeleted++
	}
	n, err := sc.store.DeleteExpiredPipelines(ctx, sc.retention)
	if err != nil {
		slog.Error("gc: delete expired pipelines", "err", err)
		return
	}
	if n > 0 || blobsDeleted > 0 {
		slog.Info("gc: retention sweep", "pipelines", n, "artifact_blobs", blobsDeleted,
			"retention", sc.retention)
	}
}

func (sc *Scheduler) step(ctx context.Context) {
	for _, op := range []struct {
		name string
		fn   func(context.Context) (int64, error)
	}{
		{"cancel_dead", sc.store.CancelDeadJobs},
		{"promote_ready", sc.store.PromoteReadyJobs},
		{"expire_blocked", sc.store.ExpireBlockedJobs},
		{"fail_overdue", sc.store.FailOverdueJobs},
		{"fail_stuck_pending", sc.store.FailStuckPending},
		{"fail_stale", func(ctx context.Context) (int64, error) {
			return sc.store.FailStaleJobs(ctx, sc.staleAfter)
		}},
	} {
		n, err := op.fn(ctx)
		if err != nil {
			slog.Error("scheduler op failed", "op", op.name, "err", err)
			continue
		}
		if n > 0 {
			slog.Info("scheduler transition", "op", op.name, "jobs", n)
		}
	}
}
