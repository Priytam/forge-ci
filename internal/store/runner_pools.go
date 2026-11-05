package store

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// RunnerPool is a set of identical runners an autoscale provider resizes.
// Settings is the provider's JSON config, encrypted at rest; the API masks
// its secret keys when reading.
type RunnerPool struct {
	ID             int64     `json:"id"`
	Name           string    `json:"name"`
	Provider       string    `json:"provider"`
	Tags           []string  `json:"tags"`
	Min            int       `json:"min_runners"`
	Max            int       `json:"max_runners"`
	JobsPerRunner  int       `json:"jobs_per_runner"`
	ScaleDownAfter int       `json:"scale_down_after_s"`
	Settings       string    `json:"settings,omitempty"` // JSON object
	Enabled        bool      `json:"enabled"`
	CreatedAt      time.Time `json:"created_at"`

	// Status, written by the loop.
	Current      int        `json:"current_runners"`
	Desired      int        `json:"desired_runners"`
	PendingJobs  int        `json:"pending_jobs"`
	LastAction   string     `json:"last_action"`
	LastError    string     `json:"last_error"`
	LastScaledAt *time.Time `json:"last_scaled_at"`
	LastBusyAt   *time.Time `json:"last_busy_at"`
	CheckedAt    *time.Time `json:"checked_at"`
}

var ErrDuplicatePool = errors.New("a pool with that name exists")

var poolNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,40}$`)

func (p *RunnerPool) validate() error {
	p.Name = strings.TrimSpace(p.Name)
	if !poolNameRe.MatchString(p.Name) {
		return errors.New("name must be lowercase letters, digits and dashes (it names the runners)")
	}
	if p.Min < 0 || p.Max < 1 || p.Max > 1000 || p.Min > p.Max {
		return errors.New("min must be >= 0, max between 1 and 1000, and min <= max")
	}
	if p.JobsPerRunner < 1 {
		p.JobsPerRunner = 1
	}
	if p.ScaleDownAfter < 60 {
		p.ScaleDownAfter = 600
	}
	tags := []string{}
	for _, t := range p.Tags {
		if t = strings.TrimSpace(t); t != "" {
			tags = append(tags, t)
		}
	}
	p.Tags = tags
	return nil
}

const poolCols = `id, name, provider, tags, min_runners, max_runners, jobs_per_runner, scale_down_after_s, settings, enabled, created_at,
	current_runners, desired_runners, pending_jobs, last_action, last_error, last_scaled_at, last_busy_at, checked_at`

func (s *Store) scanPool(row pgx.Row, reveal bool) (*RunnerPool, error) {
	var p RunnerPool
	var enc string
	if err := row.Scan(&p.ID, &p.Name, &p.Provider, &p.Tags, &p.Min, &p.Max, &p.JobsPerRunner, &p.ScaleDownAfter, &enc, &p.Enabled, &p.CreatedAt,
		&p.Current, &p.Desired, &p.PendingJobs, &p.LastAction, &p.LastError, &p.LastScaledAt, &p.LastBusyAt, &p.CheckedAt); err != nil {
		return nil, err
	}
	if reveal {
		plain, err := s.cipher.Decrypt(enc)
		if err != nil {
			return nil, err
		}
		p.Settings = plain
	}
	return &p, nil
}

// ListRunnerPools returns every pool; with reveal, settings are decrypted.
func (s *Store) ListRunnerPools(ctx context.Context, reveal bool) ([]RunnerPool, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+poolCols+` FROM runner_pools ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RunnerPool{}
	for rows.Next() {
		p, err := s.scanPool(rows, reveal)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// GetRunnerPool returns one pool with decrypted settings.
func (s *Store) GetRunnerPool(ctx context.Context, id int64) (*RunnerPool, error) {
	p, err := s.scanPool(s.pool.QueryRow(ctx, `SELECT `+poolCols+` FROM runner_pools WHERE id=$1`, id), true)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

// SaveRunnerPool inserts (ID 0) or updates a pool. Empty Settings on update
// keep the stored ones.
func (s *Store) SaveRunnerPool(ctx context.Context, p RunnerPool) (*RunnerPool, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	enc, err := s.cipher.Encrypt(p.Settings)
	if err != nil {
		return nil, err
	}
	var id int64
	if p.ID == 0 {
		err = s.pool.QueryRow(ctx,
			`INSERT INTO runner_pools (name, provider, tags, min_runners, max_runners, jobs_per_runner, scale_down_after_s, settings, enabled)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING id`,
			p.Name, p.Provider, p.Tags, p.Min, p.Max, p.JobsPerRunner, p.ScaleDownAfter, enc, p.Enabled).Scan(&id)
	} else {
		id = p.ID
		var tag pgconn.CommandTag
		tag, err = s.pool.Exec(ctx,
			`UPDATE runner_pools SET name=$2, provider=$3, tags=$4, min_runners=$5, max_runners=$6, jobs_per_runner=$7,
			        scale_down_after_s=$8, settings = CASE WHEN $9 = '' THEN settings ELSE $9 END, enabled=$10
			 WHERE id=$1`,
			p.ID, p.Name, p.Provider, p.Tags, p.Min, p.Max, p.JobsPerRunner, p.ScaleDownAfter, enc, p.Enabled)
		if err == nil && tag.RowsAffected() == 0 {
			return nil, ErrNotFound
		}
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return nil, ErrDuplicatePool
	}
	if err != nil {
		return nil, err
	}
	return s.GetRunnerPool(ctx, id)
}

func (s *Store) DeleteRunnerPool(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM runner_pools WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// PoolDemand is the queue's demand on one pool.
type PoolDemand struct {
	Pending       int      // waiting jobs whose tags the pool's runners cover
	Running       int      // jobs running on runners that cover the pool's tags
	BusyRunnerIDs []string // those runners
}

// PoolDemand computes demand for a pool: pending jobs whose required tags
// are a subset of the pool's tags, and jobs running on online runners whose
// tags cover the pool's tags (its own runners, by construction).
func (s *Store) PoolDemand(ctx context.Context, tags []string) (PoolDemand, error) {
	if tags == nil {
		tags = []string{}
	}
	var d PoolDemand
	err := s.pool.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM jobs j WHERE j.status = 'pending' AND j.tags <@ $1::text[]),
		        (SELECT count(*) FROM jobs j JOIN runners r ON r.id = j.runner_id
		          WHERE j.status = 'running' AND r.tags @> $1::text[] AND r.tags <@ $1::text[]),
		        COALESCE((SELECT array_agg(DISTINCT r.id) FROM jobs j JOIN runners r ON r.id = j.runner_id
		          WHERE j.status = 'running' AND r.tags @> $1::text[] AND r.tags <@ $1::text[]), '{}')`, tags).
		Scan(&d.Pending, &d.Running, &d.BusyRunnerIDs)
	return d, err
}

// RecordPoolStatus stores what the loop observed and did for a pool.
func (s *Store) RecordPoolStatus(ctx context.Context, id int64, current, desired, pending int, action, lastErr string, scaled, busy bool) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE runner_pools SET current_runners=$2, desired_runners=$3, pending_jobs=$4,
		        last_action = CASE WHEN $5 <> '' THEN $5 ELSE last_action END,
		        last_error=$6, checked_at=now(),
		        last_scaled_at = CASE WHEN $7 THEN now() ELSE last_scaled_at END,
		        last_busy_at = CASE WHEN $8 THEN now() ELSE last_busy_at END
		 WHERE id=$1`, id, current, desired, pending, action, lastErr, scaled, busy)
	return err
}

// TryAdvisoryLock takes a session advisory lock named key on a dedicated
// connection; release returns the connection (and the lock). ok is false
// when another holder has it.
func (s *Store) TryAdvisoryLock(ctx context.Context, key string) (release func(), ok bool, err error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext($1))`, key).Scan(&ok); err != nil {
		conn.Release()
		return nil, false, err
	}
	if !ok {
		conn.Release()
		return nil, false, nil
	}
	return func() {
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtext($1))`, key)
		conn.Release()
	}, true, nil
}
