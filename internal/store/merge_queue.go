package store

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// The merge queue (batch 7, phase F): per repo and target branch, changes
// are tested on top of the target one at a time and merged in order.

// QueueEntry is one change's place in a merge queue.
type QueueEntry struct {
	ID           int64      `json:"id"`
	ChangeID     int64      `json:"change_id"`
	Repo         string     `json:"repo"`
	TargetBranch string     `json:"target_branch"`
	HeadSHA      string     `json:"head_sha"`
	Method       string     `json:"method"`
	State        string     `json:"state"` // queued | testing | merging | merged | failed | removed
	BaseSHA      string     `json:"base_sha,omitempty"`
	TestSHA      string     `json:"test_sha,omitempty"`
	PipelineID   *int64     `json:"pipeline_id,omitempty"`
	OutboxID     *int64     `json:"outbox_id,omitempty"`
	Error        string     `json:"error,omitempty"`
	RequestedBy  string     `json:"requested_by,omitempty"`
	EnqueuedAt   time.Time  `json:"enqueued_at"`
	StartedAt    *time.Time `json:"started_at,omitempty"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
	// Listing only.
	Position int    `json:"position,omitempty"`
	Number   string `json:"number,omitempty"`
	Title    string `json:"title,omitempty"`
}

// QueueActive are the states of an entry still in its queue.
var QueueActive = []string{"queued", "testing", "merging"}

// ErrAlreadyQueued: the change is in its queue already.
var ErrAlreadyQueued = errors.New("the change is already in the merge queue")

const queueCols = `q.id, q.change_id, q.repo, q.target_branch, q.head_sha, q.method, q.state, q.base_sha, q.test_sha,
	q.pipeline_id, q.outbox_id, q.error, q.requested_by, q.enqueued_at, q.started_at, q.finished_at`

func scanQueue(row pgx.Row, e *QueueEntry, extra ...any) error {
	return row.Scan(append([]any{&e.ID, &e.ChangeID, &e.Repo, &e.TargetBranch, &e.HeadSHA, &e.Method, &e.State,
		&e.BaseSHA, &e.TestSHA, &e.PipelineID, &e.OutboxID, &e.Error, &e.RequestedBy, &e.EnqueuedAt,
		&e.StartedAt, &e.FinishedAt}, extra...)...)
}

// touchChange marks the change's gate for re-evaluation and wakes its pages.
func (s *Store) touchChange(ctx context.Context, changeID int64) error {
	if _, err := s.pool.Exec(ctx, `UPDATE changes SET gate_dirty_at = now() WHERE id = $1`, changeID); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `SELECT pg_notify($1, $2)`, changesChannel, strconv.FormatInt(changeID, 10))
	return err
}

// EnqueueMerge adds a change to the end of its target's queue.
func (s *Store) EnqueueMerge(ctx context.Context, e QueueEntry) (*QueueEntry, error) {
	var out QueueEntry
	err := scanQueue(s.pool.QueryRow(ctx,
		`INSERT INTO merge_queue AS q (change_id, repo, target_branch, head_sha, method, requested_by)
		 VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING RETURNING `+queueCols,
		e.ChangeID, e.Repo, e.TargetBranch, e.HeadSHA, e.Method, e.RequestedBy), &out)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAlreadyQueued
	}
	if err != nil {
		return nil, err
	}
	return &out, s.touchChange(ctx, e.ChangeID)
}

// ActiveQueueEntry returns the change's live entry and its position (1 =
// next), or nil.
func (s *Store) ActiveQueueEntry(ctx context.Context, changeID int64) (*QueueEntry, error) {
	var e QueueEntry
	err := scanQueue(s.pool.QueryRow(ctx,
		`SELECT `+queueCols+`, (SELECT count(*) FROM merge_queue o WHERE o.repo = q.repo
		     AND o.target_branch = q.target_branch AND o.state = ANY($2) AND o.id <= q.id)
		 FROM merge_queue q WHERE q.change_id = $1 AND q.state = ANY($2)`, changeID, QueueActive), &e, &e.Position)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &e, err
}

// LatestQueueEntry returns the change's newest entry, live or finished.
func (s *Store) LatestQueueEntry(ctx context.Context, changeID int64) (*QueueEntry, error) {
	var e QueueEntry
	err := scanQueue(s.pool.QueryRow(ctx,
		`SELECT `+queueCols+` FROM merge_queue q WHERE q.change_id = $1 ORDER BY q.id DESC LIMIT 1`, changeID), &e)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &e, err
}

// ListQueue returns a queue's live entries in order (every queue's when
// repo and target are empty).
func (s *Store) ListQueue(ctx context.Context, repo, target string) ([]QueueEntry, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+queueCols+`, c.number, c.title,
		        row_number() OVER (PARTITION BY q.repo, q.target_branch ORDER BY q.id)
		 FROM merge_queue q JOIN changes c ON c.id = q.change_id
		 WHERE ($1 = '' OR q.repo = $1) AND ($2 = '' OR q.target_branch = $2) AND q.state = ANY($3)
		 ORDER BY q.repo, q.target_branch, q.id`, repo, target, QueueActive)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []QueueEntry{}
	for rows.Next() {
		var e QueueEntry
		if err := scanQueue(rows, &e, &e.Number, &e.Title, &e.Position); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// QueueHeads returns the first live entry of every queue.
func (s *Store) QueueHeads(ctx context.Context) ([]QueueEntry, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT ON (q.repo, q.target_branch) `+queueCols+` FROM merge_queue q
		 WHERE q.state = ANY($1) ORDER BY q.repo, q.target_branch, q.id`, QueueActive)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QueueEntry
	for rows.Next() {
		var e QueueEntry
		if err := scanQueue(rows, &e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// WithQueueLock runs fn while holding repo/target's queue lock, so one
// replica advances a queue at a time. ran is false when another holds it.
func (s *Store) WithQueueLock(ctx context.Context, repo, target string, fn func() error) (ran bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtext('merge_queue:' || $1 || ':' || $2))`,
		repo, target).Scan(&ran); err != nil || !ran {
		return false, err
	}
	if err := fn(); err != nil {
		return true, err
	}
	return true, tx.Commit(ctx)
}

// UpdateQueueEntry saves an entry's progress (state, base, test commit,
// pipeline, outbox item, error), stamping when it started and finished.
func (s *Store) UpdateQueueEntry(ctx context.Context, e QueueEntry) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE merge_queue SET state = $2, base_sha = $3, test_sha = $4, pipeline_id = $5, outbox_id = $6,
		     error = $7, updated_at = now(),
		     started_at = CASE WHEN $2 <> 'queued' THEN COALESCE(started_at, now()) END,
		     finished_at = CASE WHEN $2 IN ('merged','failed','removed') THEN now() END
		 WHERE id = $1`,
		e.ID, e.State, e.BaseSHA, e.TestSHA, e.PipelineID, e.OutboxID, truncate(e.Error, 500))
	if err != nil {
		return err
	}
	return s.touchChange(ctx, e.ChangeID)
}

// RemoveFromQueue takes the change's live entry out of its queue.
func (s *Store) RemoveFromQueue(ctx context.Context, changeID int64, reason string) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE merge_queue SET state = 'removed', error = $2, updated_at = now(), finished_at = now()
		 WHERE change_id = $1 AND state = ANY($3)`, changeID, truncate(reason, 500), QueueActive)
	if err != nil || tag.RowsAffected() == 0 {
		return false, err
	}
	return true, s.touchChange(ctx, changeID)
}

// PipelineStatus returns a pipeline's status ("" when it is gone).
func (s *Store) PipelineStatus(ctx context.Context, id int64) (string, error) {
	var st string
	err := s.pool.QueryRow(ctx, `SELECT status FROM pipelines WHERE id = $1`, id).Scan(&st)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return st, err
}

// OutboxItemByID returns one outbox item.
func (s *Store) OutboxItemByID(ctx context.Context, id int64) (*OutboxItem, error) {
	o, err := scanOutbox(s.pool.QueryRow(ctx, `SELECT `+outboxCols+` FROM provider_outbox WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return o, err
}
