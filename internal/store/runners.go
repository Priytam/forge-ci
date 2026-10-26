package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

const onlineWindow = 30 * time.Second

// staleWindow: a runner not seen for this long is stale — it no longer counts
// toward capacity or tag totals, and an admin may remove it.
const staleWindow = 24 * time.Hour

// ErrRunnerActive: the runner is online or still running jobs, so it can't be
// removed.
var ErrRunnerActive = errors.New("runner is online or running jobs")

// TouchRunner upserts a runner on every acquire poll and reports whether it
// is paused (paused runners must not receive jobs).
func (s *Store) TouchRunner(ctx context.Context, id, executor string, tags []string) (paused bool, err error) {
	if tags == nil {
		tags = []string{}
	}
	err = s.pool.QueryRow(ctx,
		`INSERT INTO runners (id, executor, tags)
		 VALUES ($1,$2,$3)
		 ON CONFLICT (id) DO UPDATE
		   SET last_contact_at = now(), executor = EXCLUDED.executor, tags = EXCLUDED.tags
		 RETURNING paused`, id, executor, tags).Scan(&paused)
	return paused, err
}

func (s *Store) ListRunners(ctx context.Context) ([]proto.Runner, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT r.id, r.executor, r.tags, r.description, r.paused, r.created_at, r.last_contact_at,
		        r.version, r.draining, r.update_requested_at IS NOT NULL,
		        (SELECT count(*) FROM jobs j WHERE j.status='running' AND j.runner_id = r.id),
		        r.concurrency
		 FROM runners r ORDER BY r.last_contact_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []proto.Runner{}
	for rows.Next() {
		var r proto.Runner
		if err := rows.Scan(&r.ID, &r.Executor, &r.Tags, &r.Description, &r.Paused,
			&r.CreatedAt, &r.LastContactAt, &r.Version, &r.Draining, &r.UpdateRequested, &r.RunningJobs, &r.Concurrency); err != nil {
			return nil, err
		}
		r.Online = time.Since(r.LastContactAt) < onlineWindow
		r.Stale = time.Since(r.LastContactAt) >= staleWindow
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteRunner removes a runner that is offline and running nothing. Its
// jobs and history keep the runner id; a runner that comes back simply
// registers again.
func (s *Store) DeleteRunner(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM runners r WHERE r.id = $1 AND r.last_contact_at < now() - $2::interval
		   AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.runner_id = r.id AND j.status = 'running')`,
		id, onlineWindow.String())
	if err != nil {
		return err
	}
	if tag.RowsAffected() > 0 {
		return nil
	}
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM runners WHERE id = $1)`, id).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return ErrRunnerActive
	}
	return ErrNotFound
}

func (s *Store) SetRunnerPaused(ctx context.Context, id string, paused bool) (*proto.Runner, error) {
	var r proto.Runner
	err := s.pool.QueryRow(ctx,
		`UPDATE runners SET paused=$2 WHERE id=$1
		 RETURNING id, executor, tags, description, paused, created_at, last_contact_at`,
		id, paused).Scan(&r.ID, &r.Executor, &r.Tags, &r.Description, &r.Paused,
		&r.CreatedAt, &r.LastContactAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	r.Online = time.Since(r.LastContactAt) < onlineWindow
	return &r, err
}
