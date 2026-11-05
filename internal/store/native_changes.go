package store

import (
	"context"
	"errors"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Forge-native changes (batch 7, phase F): on a host with no pull requests
// (plain git) the change itself lives in Forge. It is created here, its two
// branch tips are read by the refresh worker, and it is merged by a runner.

// NativeChange is what creating a native change needs.
type NativeChange struct {
	Repo         string
	Provider     string
	Author       string
	Title        string
	Body         string
	SourceBranch string
	TargetBranch string
	Draft        bool
}

// ErrDuplicateChange: an open change already proposes the same branches.
var ErrDuplicateChange = errors.New("an open change from this branch into the target already exists")

// CreateNativeChange stores a new open change numbered after the repo's
// last one. Its commits are read by the first refresh.
func (s *Store) CreateNativeChange(ctx context.Context, c NativeChange) (id int64, number string, err error) {
	for attempt := 0; attempt < 5; attempt++ {
		var exists bool
		if err := s.pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM changes WHERE repo = $1 AND source_branch = $2 AND target_branch = $3
			                AND state = 'open')`, c.Repo, c.SourceBranch, c.TargetBranch).Scan(&exists); err != nil {
			return 0, "", err
		}
		if exists {
			return 0, "", ErrDuplicateChange
		}
		err = s.pool.QueryRow(ctx,
			`INSERT INTO changes (repo, provider, number, title, body, author, source_branch, target_branch,
			                      state, draft, created_at, updated_at, synced_at, gate_dirty_at)
			 SELECT $1, $2, (COALESCE(MAX(number::bigint), 0) + 1)::text, $3, $4, $5, $6, $7, 'open', $8,
			        now(), now(), now(), now()
			 FROM changes WHERE repo = $1 AND number ~ '^[0-9]{1,18}$'
			 RETURNING id, number`,
			c.Repo, c.Provider, c.Title, c.Body, c.Author, c.SourceBranch, c.TargetBranch, c.Draft).Scan(&id, &number)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			continue // someone took the number: take the next one
		}
		if err != nil {
			return 0, "", err
		}
		_, err = s.pool.Exec(ctx, `SELECT pg_notify($1, $2)`, changesChannel, strconv.FormatInt(id, 10))
		return id, number, err
	}
	return 0, "", errors.New("could not number the change: try again")
}

// NativeEdit is a change to a native change's own fields (nil = keep).
type NativeEdit struct {
	Title *string
	Body  *string
	Draft *bool
	State *string // open | closed
}

// EditNativeChange updates a native change; closing it also takes it out of
// its merge queue.
func (s *Store) EditNativeChange(ctx context.Context, id int64, e NativeEdit) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE changes SET title = COALESCE($2, title), body = COALESCE($3, body), draft = COALESCE($4, draft),
		     state = COALESCE($5, state), updated_at = now(), gate_dirty_at = now()
		 WHERE id = $1 AND state <> 'merged'`, id, e.Title, e.Body, e.Draft, e.State)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if e.State != nil && *e.State == "closed" {
		if _, err := s.RemoveFromQueue(ctx, id, "the change was closed"); err != nil {
			return err
		}
	}
	_, err = s.pool.Exec(ctx, `SELECT pg_notify($1, $2)`, changesChannel, strconv.FormatInt(id, 10))
	return err
}

// MarkNativeMerged records that a runner merged a native change.
func (s *Store) MarkNativeMerged(ctx context.Context, id int64, targetTip string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE changes SET state = 'merged', base_sha = COALESCE(NULLIF($2, ''), base_sha), updated_at = now()
		 WHERE id = $1 AND state = 'open'`, id, targetTip)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	_, err = s.pool.Exec(ctx, `SELECT pg_notify($1, $2)`, changesChannel, strconv.FormatInt(id, 10))
	return err
}

// NativeChangesOn returns the open native changes of repo whose source or
// target is branch (a push to it moves them).
func (s *Store) NativeChangesOn(ctx context.Context, repo, branch string) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT number FROM changes WHERE repo = $1 AND state = 'open' AND (source_branch = $2 OR target_branch = $2)`,
		repo, branch)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
