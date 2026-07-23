package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

const onlineWindow = 30 * time.Second

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
		`SELECT id, executor, tags, description, paused, created_at, last_contact_at
		 FROM runners ORDER BY last_contact_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []proto.Runner{}
	for rows.Next() {
		var r proto.Runner
		if err := rows.Scan(&r.ID, &r.Executor, &r.Tags, &r.Description, &r.Paused,
			&r.CreatedAt, &r.LastContactAt); err != nil {
			return nil, err
		}
		r.Online = time.Since(r.LastContactAt) < onlineWindow
		out = append(out, r)
	}
	return out, rows.Err()
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
