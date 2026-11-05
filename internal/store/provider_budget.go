package store

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/priytamjeepandey/forge-ci/internal/vcs"
)

// Provider API budget: one token bucket per credential, shared by every
// replica because it lives in Postgres (a row lock, no Redis). The bucket
// refills at capacity per window and is overwritten by the host's own
// rate-limit headers whenever it sends them, so Forge spends what the host
// says is left, not what it guesses.

// budgetReserve is the share of capacity each class must leave untouched:
// when a bucket runs low, backfill stops first, then reconcile, then webhook
// refreshes. User actions never wait.
var budgetReserve = map[string]float64{
	vcs.ClassUser:      0,
	vcs.ClassWebhook:   0.05,
	vcs.ClassReconcile: 0.20,
	vcs.ClassBackfill:  0.50,
}

// defaultCapacity is a provider's hourly budget until its headers say
// otherwise (GitHub: 5,000/hour per installation or token).
func defaultCapacity(provider string) int {
	switch provider {
	case "github":
		return 5000
	case "gitlab", "gitea":
		return 2000
	case "bitbucket":
		return 1000
	}
	return 3600
}

// AcquireProviderBudget takes one call from credential's bucket for class.
// ok=false means the class must wait (wait is until enough has refilled or
// the host's window resets); the token is not taken then.
func (s *Store) AcquireProviderBudget(ctx context.Context, credential, provider, class string) (ok bool, wait time.Duration, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, 0, err
	}
	defer tx.Rollback(ctx)
	capacity := defaultCapacity(provider)
	if _, err := tx.Exec(ctx,
		`INSERT INTO provider_budgets (credential, provider, capacity, tokens) VALUES ($1,$2,$3,$4)
		 ON CONFLICT (credential) DO NOTHING`, credential, provider, capacity, float64(capacity)); err != nil {
		return false, 0, err
	}
	var tokens float64
	var windowS int
	var resetAt *time.Time
	var updated, now time.Time
	if err := tx.QueryRow(ctx,
		`SELECT capacity, tokens, window_s, reset_at, updated_at, now() FROM provider_budgets
		 WHERE credential = $1 FOR UPDATE`, credential).
		Scan(&capacity, &tokens, &windowS, &resetAt, &updated, &now); err != nil {
		return false, 0, err
	}
	tokens, resetAt = refill(capacity, tokens, windowS, resetAt, updated, now)
	floor := budgetReserve[class] * float64(capacity)
	if tokens-1 < floor {
		wait = refillWait(capacity, tokens, floor+1, windowS, resetAt, now)
		_, err = tx.Exec(ctx,
			`UPDATE provider_budgets SET tokens = $2, reset_at = $3, updated_at = $4 WHERE credential = $1`,
			credential, tokens, resetAt, now)
		if err != nil {
			return false, 0, err
		}
		return false, wait, tx.Commit(ctx)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE provider_budgets SET tokens = $2, reset_at = $3, updated_at = $4 WHERE credential = $1`,
		credential, tokens-1, resetAt, now); err != nil {
		return false, 0, err
	}
	return true, 0, tx.Commit(ctx)
}

// refill brings a bucket up to now: a host window that has reset refills it
// completely; otherwise it refills continuously at capacity per window.
func refill(capacity int, tokens float64, windowS int, resetAt *time.Time, updated, now time.Time) (float64, *time.Time) {
	if resetAt != nil {
		if !now.Before(*resetAt) {
			return float64(capacity), nil
		}
		return tokens, resetAt // the host said how much is left until its reset
	}
	if windowS <= 0 {
		windowS = 3600
	}
	elapsed := now.Sub(updated).Seconds()
	return math.Min(float64(capacity), tokens+elapsed*float64(capacity)/float64(windowS)), nil
}

// refillWait is how long until the bucket holds need tokens.
func refillWait(capacity int, tokens, need float64, windowS int, resetAt *time.Time, now time.Time) time.Duration {
	if resetAt != nil {
		return resetAt.Sub(now)
	}
	if windowS <= 0 {
		windowS = 3600
	}
	secs := (need - tokens) * float64(windowS) / float64(capacity)
	return time.Duration(math.Ceil(secs)) * time.Second
}

// ObserveProviderRate overwrites a bucket with what the host's headers said.
func (s *Store) ObserveProviderRate(ctx context.Context, credential, provider string, ri vcs.RateInfo) error {
	capacity := ri.Limit
	if capacity <= 0 {
		capacity = defaultCapacity(provider)
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO provider_budgets (credential, provider, capacity, tokens, reset_at)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (credential) DO UPDATE SET capacity = EXCLUDED.capacity, tokens = EXCLUDED.tokens,
		     reset_at = EXCLUDED.reset_at, updated_at = now()`,
		credential, provider, capacity, float64(ri.Remaining), nullTime(ri.Reset))
	return err
}

// ExhaustProviderBudget empties a bucket until until (the host refused us).
func (s *Store) ExhaustProviderBudget(ctx context.Context, credential, provider string, until time.Time) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO provider_budgets (credential, provider, capacity, tokens, reset_at)
		 VALUES ($1, $2, $3, 0, $4)
		 ON CONFLICT (credential) DO UPDATE SET tokens = 0, reset_at = EXCLUDED.reset_at, updated_at = now()`,
		credential, provider, defaultCapacity(provider), until)
	return err
}

// ProviderBudget is one bucket as the About page shows it.
type ProviderBudget struct {
	Credential string     `json:"credential"`
	Provider   string     `json:"provider"`
	Capacity   int        `json:"capacity"`
	Remaining  int        `json:"remaining"`
	ResetAt    *time.Time `json:"reset_at,omitempty"`
}

// ListProviderBudgets returns every bucket, refilled to now.
func (s *Store) ListProviderBudgets(ctx context.Context) ([]ProviderBudget, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT credential, provider, capacity, tokens, window_s, reset_at, updated_at, now()
		 FROM provider_budgets ORDER BY credential`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProviderBudget{}
	for rows.Next() {
		var b ProviderBudget
		var tokens float64
		var windowS int
		var updated, now time.Time
		if err := rows.Scan(&b.Credential, &b.Provider, &b.Capacity, &tokens, &windowS, &b.ResetAt, &updated, &now); err != nil {
			return nil, err
		}
		tokens, b.ResetAt = refill(b.Capacity, tokens, windowS, b.ResetAt, updated, now)
		b.Remaining = int(tokens)
		out = append(out, b)
	}
	return out, rows.Err()
}

// ---- call accounting ----

// RecordProviderCall counts one provider call in the hourly table.
func (s *Store) RecordProviderCall(ctx context.Context, info vcs.CallInfo) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO provider_call_stats (hour, provider, class, result, read_path, calls)
		 VALUES (date_trunc('hour', now()), $1, $2, $3, $4, 1)
		 ON CONFLICT (hour, provider, class, result, read_path) DO UPDATE SET calls = provider_call_stats.calls + 1`,
		info.Provider, info.Class, info.Result, info.ReadPath)
	return err
}

// ProviderCallCount is calls in a window for one provider/class/result.
type ProviderCallCount struct {
	Provider string `json:"provider"`
	Class    string `json:"class"`
	Result   string `json:"result"`
	ReadPath bool   `json:"read_path"`
	Calls    int64  `json:"calls"`
}

// ProviderCallsSince sums provider calls from since (whole hours).
func (s *Store) ProviderCallsSince(ctx context.Context, since time.Time) ([]ProviderCallCount, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT provider, class, result, read_path, sum(calls)::bigint FROM provider_call_stats
		 WHERE hour >= date_trunc('hour', $1::timestamptz)
		 GROUP BY 1,2,3,4 ORDER BY 1,2,3,4`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProviderCallCount{}
	for rows.Next() {
		var c ProviderCallCount
		if err := rows.Scan(&c.Provider, &c.Class, &c.Result, &c.ReadPath, &c.Calls); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// OpenChangeCount is how many changes are open, for calls-per-change.
func (s *Store) OpenChangeCount(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM changes WHERE state = 'open' AND synced_at IS NOT NULL`).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return n, err
}

// PruneProviderCallStats drops hourly rows older than keep.
func (s *Store) PruneProviderCallStats(ctx context.Context, keep time.Duration) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM provider_call_stats WHERE hour < now() - $1 * interval '1 second'`, keep.Seconds())
	return err
}
