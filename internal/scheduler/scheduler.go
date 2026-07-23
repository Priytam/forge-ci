// Package scheduler drives the job state machine. Every transition it applies
// is an idempotent SQL statement, so running it after a crash — or running two
// schedulers concurrently — cannot corrupt state.
package scheduler

import (
	"context"
	"log/slog"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/store"
)

type Scheduler struct {
	store      *store.Store
	tick       time.Duration
	staleAfter time.Duration
}

func New(s *store.Store) *Scheduler {
	return &Scheduler{store: s, tick: time.Second, staleAfter: 90 * time.Second}
}

func (sc *Scheduler) Run(ctx context.Context) {
	t := time.NewTicker(sc.tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sc.step(ctx)
		}
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
