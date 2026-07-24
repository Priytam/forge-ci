package logstore

import (
	"context"
	"sync"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/store"
)

// pgBackend is the no-Redis fallback: log bodies live in the job_logs table
// exactly as before. It has no pub/sub, so SSE degrades to a poll loop. Reads
// go through the store's masked GetLogs; there is no separate archive step
// (bodies are already durable in Postgres).
//
// The masking carry-over tail is kept in process memory here: the postgres
// backend is the single-instance dev path, so there is no cross-replica
// sharing to worry about (the redis backend handles that case in Redis).
type pgBackend struct {
	store *store.Store

	tmu   sync.Mutex
	tails map[int64]string
}

func newPGBackend(st *store.Store) *pgBackend {
	return &pgBackend{store: st, tails: map[int64]string{}}
}

func (b *pgBackend) GetTail(_ context.Context, jobID int64) (string, error) {
	b.tmu.Lock()
	defer b.tmu.Unlock()
	return b.tails[jobID], nil
}

func (b *pgBackend) SetTail(_ context.Context, jobID int64, tail string) error {
	b.tmu.Lock()
	defer b.tmu.Unlock()
	if tail == "" {
		delete(b.tails, jobID)
	} else {
		b.tails[jobID] = tail
	}
	return nil
}

func (b *pgBackend) Kind() string { return "postgres" }

func (b *pgBackend) Append(ctx context.Context, jobID int64, p []byte, capBytes int64) (AppendResult, error) {
	total, truncNow, err := b.store.AppendLogCapped(ctx, jobID, string(p), capBytes)
	if err != nil {
		return AppendResult{}, err
	}
	return AppendResult{Total: total, TruncatedNow: truncNow}, nil
}

func (b *pgBackend) ReadFrom(ctx context.Context, jobID int64, offset int64) ([]byte, int64, error) {
	full, err := b.store.GetLogs(ctx, jobID) // masked
	if err != nil {
		return nil, 0, err
	}
	total := int64(len(full))
	if offset < 0 {
		offset = 0
	}
	if offset > total {
		offset = total
	}
	return []byte(full[offset:]), total, nil
}

func (b *pgBackend) Snapshot(ctx context.Context, jobID int64) ([]byte, error) {
	full, err := b.store.GetLogs(ctx, jobID)
	if err != nil {
		return nil, err
	}
	return []byte(full), nil
}

func (b *pgBackend) Truncated(ctx context.Context, jobID int64) (bool, error) {
	meta, err := b.store.GetJobLogMeta(ctx, jobID)
	if err != nil {
		return false, err
	}
	return meta.Truncated, nil
}

// Evict is a no-op: postgres bodies are the source of truth and are removed only
// by retention GC (cascade on pipeline delete).
func (b *pgBackend) Evict(context.Context, int64, time.Duration) error { return nil }

// Subscribe reports no native pub/sub; the SSE handler polls instead.
func (b *pgBackend) Subscribe(context.Context, int64) (Subscription, bool) { return nil, false }

func (b *pgBackend) Close() error { return nil }
