package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
	"github.com/priytamjeepandey/forge-ci/internal/vcs"
)

// Diffs (batch 7, phase C): computed by runners from their git mirrors,
// once per (from, to, mode), stored in the blob store; and the runner task
// queue that carries the work.

// DiffKey names a diff. Mode "change": from the merge base of From and To;
// "range": From..To directly.
type DiffKey struct {
	Repo string
	From string
	To   string
	Mode string
}

func (k DiffKey) dedupe() string { return "diff:" + k.Repo + ":" + k.Mode + ":" + k.From + ".." + k.To }

// ChangeDiff is a diff's row.
type ChangeDiff struct {
	DiffKey
	State      string     `json:"state"` // pending | ready | failed
	MergeBase  string     `json:"merge_base"`
	BlobPrefix string     `json:"-"`
	Chunks     int        `json:"chunks"`
	Files      int        `json:"files"`
	Additions  int        `json:"additions"`
	Deletions  int        `json:"deletions"`
	Bytes      int64      `json:"bytes"`
	Fetched    bool       `json:"fetched"`
	ComputeMS  int        `json:"compute_ms"`
	Error      string     `json:"error,omitempty"`
	Requested  time.Time  `json:"requested_at"`
	ReadyAt    *time.Time `json:"ready_at,omitempty"`
}

// mirrorAffinity is the runner_affinity key for a repo's mirror.
func mirrorAffinity(repo string) string { return "mirror:" + repo }

// requestDiffTx ensures a diff row exists and, unless it is ready, a runner
// task computes it. Repeat calls cost one no-op insert each. retryFailed
// re-arms a failed diff (someone opened it again).
func requestDiffTx(ctx context.Context, tx pgx.Tx, k DiffKey, refs []string, retryFailed bool) error {
	tag, err := tx.Exec(ctx,
		`INSERT INTO change_diffs (repo, from_sha, to_sha, mode) VALUES ($1,$2,$3,$4) ON CONFLICT DO NOTHING`,
		k.Repo, k.From, k.To, k.Mode)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		if !retryFailed {
			return nil
		}
		tag, err = tx.Exec(ctx,
			`UPDATE change_diffs SET state = 'pending', error = '', requested_at = now()
			 WHERE repo = $1 AND from_sha = $2 AND to_sha = $3 AND mode = $4 AND state = 'failed'`,
			k.Repo, k.From, k.To, k.Mode)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
	}
	payload, _ := json.Marshal(proto.DiffTask{From: k.From, To: k.To, Mode: k.Mode, FetchRefs: refs})
	_, err = tx.Exec(ctx,
		`INSERT INTO runner_tasks (kind, repo, affinity_key, dedupe_key, payload) VALUES ($1,$2,$3,$4,$5)
		 ON CONFLICT (dedupe_key) DO UPDATE SET state = 'pending', attempts = 0, error = '', payload = EXCLUDED.payload,
		     queued_at = now(), not_before = now(), runner_id = '', lease_until = NULL
		 WHERE runner_tasks.state IN ('failed','done')`,
		proto.TaskKindDiff, k.Repo, mirrorAffinity(k.Repo), k.dedupe(), payload)
	return err
}

// requestChangeDiffTx asks for the diff a change shows by default (its
// target tip to its head), when the host can be mirrored.
func requestChangeDiffTx(ctx context.Context, tx pgx.Tx, repo, provider string, ch *vcs.Change) error {
	if ch.BaseSHA == "" || ch.HeadSHA == "" || ch.State != "open" {
		return nil
	}
	refs, ok := vcs.MirrorRefsFor(provider, ch.ID, ch.SourceBranch, ch.TargetBranch)
	if !ok {
		return nil
	}
	return requestDiffTx(ctx, tx, DiffKey{Repo: repo, From: ch.BaseSHA, To: ch.HeadSHA, Mode: "change"}, refs, false)
}

// RequestDiff ensures a diff is ready or being computed, and returns its row.
func (s *Store) RequestDiff(ctx context.Context, k DiffKey, refs []string, retryFailed bool) (*ChangeDiff, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if err := requestDiffTx(ctx, tx, k, refs, retryFailed); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.GetDiff(ctx, k)
}

const diffCols = `repo, from_sha, to_sha, mode, state, merge_base, blob_prefix, chunks, files, additions,
	deletions, bytes, fetched, compute_ms, error, requested_at, ready_at`

func scanDiff(row pgx.Row) (*ChangeDiff, error) {
	var d ChangeDiff
	err := row.Scan(&d.Repo, &d.From, &d.To, &d.Mode, &d.State, &d.MergeBase, &d.BlobPrefix, &d.Chunks,
		&d.Files, &d.Additions, &d.Deletions, &d.Bytes, &d.Fetched, &d.ComputeMS, &d.Error, &d.Requested, &d.ReadyAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &d, err
}

// GetDiff returns a diff's row (ErrNotFound when never requested).
func (s *Store) GetDiff(ctx context.Context, k DiffKey) (*ChangeDiff, error) {
	return scanDiff(s.pool.QueryRow(ctx, `SELECT `+diffCols+` FROM change_diffs
		 WHERE repo = $1 AND from_sha = $2 AND to_sha = $3 AND mode = $4`, k.Repo, k.From, k.To, k.Mode))
}

// notifyDiffTx tells pages showing a change with this head that its diff
// moved.
func notifyDiffTx(ctx context.Context, tx pgx.Tx, k DiffKey) error {
	_, err := tx.Exec(ctx,
		`SELECT pg_notify($1, c.id::text) FROM changes c
		 WHERE c.repo = $2 AND EXISTS (SELECT 1 FROM change_revisions r WHERE r.change_id = c.id AND r.head_sha = $3)`,
		changesChannel, k.Repo, k.To)
	return err
}

// DiffReady is what storing a computed diff recorded.
type DiffReady struct {
	MergeBase  string
	BlobPrefix string
	Chunks     int
	Files      int
	Additions  int
	Deletions  int
	Bytes      int64
	Fetched    bool
	ComputeMS  int
	// Paths the diff touched ("change" diffs; renames list both paths) and
	// the CODEOWNERS file at its target tip, for the code owners rule.
	Paths          []string
	OwnersRead     bool
	CodeOwnersPath string
	CodeOwners     string
}

// MarkDiffReady records a computed diff (its bytes already in the blob
// store), wakes the pages showing it, and marks for re-evaluation the gates
// of changes whose code owners it answers.
func (s *Store) MarkDiffReady(ctx context.Context, k DiffKey, r DiffReady) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx,
		`UPDATE change_diffs SET state = 'ready', merge_base = $5, blob_prefix = $6, chunks = $7, files = $8,
		     additions = $9, deletions = $10, bytes = $11, fetched = $12, compute_ms = $13, error = '', ready_at = now(),
		     paths = $14, owners_read = $15, codeowners_path = $16, codeowners = $17
		 WHERE repo = $1 AND from_sha = $2 AND to_sha = $3 AND mode = $4`,
		k.Repo, k.From, k.To, k.Mode, r.MergeBase, r.BlobPrefix, r.Chunks, r.Files, r.Additions, r.Deletions,
		r.Bytes, r.Fetched, r.ComputeMS, r.Paths, r.OwnersRead, r.CodeOwnersPath, r.CodeOwners); err != nil {
		return err
	}
	if k.Mode == "change" {
		if _, err := tx.Exec(ctx,
			`UPDATE changes SET gate_dirty_at = now()
			 WHERE repo = $1 AND base_sha = $2 AND head_sha = $3 AND state = 'open'`, k.Repo, k.From, k.To); err != nil {
			return err
		}
	}
	if err := notifyDiffTx(ctx, tx, k); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// MarkDiffFailed records why a diff could not be computed.
func (s *Store) MarkDiffFailed(ctx context.Context, k DiffKey, reason string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx,
		`UPDATE change_diffs SET state = 'failed', error = $5
		 WHERE repo = $1 AND from_sha = $2 AND to_sha = $3 AND mode = $4 AND state <> 'ready'`,
		k.Repo, k.From, k.To, k.Mode, truncate(reason, 300)); err != nil {
		return err
	}
	if err := notifyDiffTx(ctx, tx, k); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// TouchDiffRead stamps a diff as read (at most hourly, so reads stay cheap);
// retention keeps what people still open.
func (s *Store) TouchDiffRead(ctx context.Context, k DiffKey) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE change_diffs SET last_read_at = now()
		 WHERE repo = $1 AND from_sha = $2 AND to_sha = $3 AND mode = $4
		   AND (last_read_at IS NULL OR last_read_at < now() - interval '1 hour')`, k.Repo, k.From, k.To, k.Mode)
	return err
}

// RecordDiffRead counts one diff open by result (hit | miss | pending | failed).
func (s *Store) RecordDiffRead(ctx context.Context, result string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO diff_read_stats (hour, result, reads) VALUES (date_trunc('hour', now()), $1, 1)
		 ON CONFLICT (hour, result) DO UPDATE SET reads = diff_read_stats.reads + 1`, result)
	return err
}

// DiffStats is the diff footprint over a window.
type DiffStats struct {
	Reads        map[string]int64 `json:"reads"` // by result
	HitRatio     float64          `json:"hit_ratio"`
	Computed     int64            `json:"computed"`
	ComputeP95MS float64          `json:"compute_p95_ms"`
	Fetches      int64            `json:"mirror_fetches"`
	StoredBytes  int64            `json:"stored_bytes"`
	TaskBacklog  int              `json:"task_backlog"`
	OldestTaskS  float64          `json:"oldest_task_seconds"`
}

// DiffStatsSince reports diff reads, computes and the task backlog.
func (s *Store) DiffStatsSince(ctx context.Context, since time.Time) (DiffStats, error) {
	st := DiffStats{Reads: map[string]int64{}}
	rows, err := s.pool.Query(ctx,
		`SELECT result, sum(reads)::bigint FROM diff_read_stats WHERE hour >= date_trunc('hour', $1::timestamptz) GROUP BY 1`, since)
	if err != nil {
		return st, err
	}
	for rows.Next() {
		var r string
		var n int64
		if err := rows.Scan(&r, &n); err != nil {
			rows.Close()
			return st, err
		}
		st.Reads[r] = n
	}
	rows.Close()
	// Every open that did not find the diff ready counts against the ratio.
	if total := st.Reads["hit"] + st.Reads["miss"] + st.Reads["pending"] + st.Reads["failed"]; total > 0 {
		st.HitRatio = float64(st.Reads["hit"]) / float64(total)
	}
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*), COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY compute_ms), 0),
		        count(*) FILTER (WHERE fetched)
		 FROM change_diffs WHERE state = 'ready' AND ready_at >= $1`, since).
		Scan(&st.Computed, &st.ComputeP95MS, &st.Fetches); err != nil {
		return st, err
	}
	if err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(sum(bytes), 0)::bigint FROM (SELECT DISTINCT blob_prefix, bytes FROM change_diffs WHERE state = 'ready') d`).
		Scan(&st.StoredBytes); err != nil {
		return st, err
	}
	var secs float64
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*), COALESCE(EXTRACT(EPOCH FROM now() - min(queued_at)), 0)
		 FROM runner_tasks WHERE state IN ('pending','running')`).Scan(&st.TaskBacklog, &secs); err != nil {
		return st, err
	}
	st.OldestTaskS = secs
	return st, nil
}

// ExpiredDiff is a stored diff past retention, and whether its blobs are
// still used by another row (same merge base and head).
type ExpiredDiff struct {
	Key        DiffKey
	BlobPrefix string
	Chunks     int
	Shared     bool
}

// ExpireDiffs removes diffs nobody has opened for keep and no open change
// shows, returning them so the caller deletes their blobs (unless shared).
// An expired diff is recomputed if someone opens it again.
func (s *Store) ExpireDiffs(ctx context.Context, keep time.Duration, limit int) ([]ExpiredDiff, error) {
	rows, err := s.pool.Query(ctx,
		`DELETE FROM change_diffs d
		 WHERE (d.repo, d.from_sha, d.to_sha, d.mode) IN (
		     SELECT repo, from_sha, to_sha, mode FROM change_diffs x
		     WHERE COALESCE(x.last_read_at, x.ready_at, x.requested_at) < now() - $1 * interval '1 second'
		       AND NOT EXISTS (SELECT 1 FROM changes c JOIN change_revisions r ON r.change_id = c.id
		                       WHERE c.repo = x.repo AND c.state = 'open' AND r.head_sha = x.to_sha)
		     LIMIT $2)
		 RETURNING d.repo, d.from_sha, d.to_sha, d.mode, d.blob_prefix, d.chunks`, keep.Seconds(), limit)
	if err != nil {
		return nil, err
	}
	var out []ExpiredDiff
	for rows.Next() {
		var e ExpiredDiff
		if err := rows.Scan(&e.Key.Repo, &e.Key.From, &e.Key.To, &e.Key.Mode, &e.BlobPrefix, &e.Chunks); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].BlobPrefix == "" {
			continue
		}
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM change_diffs WHERE blob_prefix = $1)`,
			out[i].BlobPrefix).Scan(&out[i].Shared); err != nil {
			return nil, err
		}
	}
	_, err = s.pool.Exec(ctx,
		`DELETE FROM runner_tasks WHERE state IN ('done','failed') AND finished_at < now() - interval '7 days'`)
	return out, err
}

// ---- runner tasks ----

// TaskRow is a claimed runner task.
type TaskRow struct {
	ID       int64
	Kind     string
	Repo     string
	Payload  json.RawMessage
	Attempts int
}

const maxTaskAttempts = 3

// ClaimRunnerTask hands runnerID the next task of kinds. A task whose repo
// mirror lives on another online runner is left for that runner until it
// has waited affinityWait (as job cache affinity does); a lease that runs
// out (the runner died) makes it claimable again.
func (s *Store) ClaimRunnerTask(ctx context.Context, runnerID string, kinds []string, affinityWait, lease time.Duration) (*TaskRow, error) {
	var t TaskRow
	err := s.pool.QueryRow(ctx,
		`UPDATE runner_tasks SET state = 'running', runner_id = $1, attempts = attempts + 1,
		     started_at = now(), lease_until = now() + $4 * interval '1 millisecond'
		 WHERE id = (
		     SELECT t.id FROM runner_tasks t
		     LEFT JOIN runner_affinity a ON a.key = t.affinity_key
		     WHERE t.kind = ANY($2) AND t.not_before <= now()
		       AND (t.state = 'pending' OR (t.state = 'running' AND t.lease_until < now()))
		       AND (a.runner_id IS NULL OR a.runner_id = $1
		            OR t.queued_at < now() - make_interval(secs => $3)
		            OR NOT EXISTS (SELECT 1 FROM runners r WHERE r.id = a.runner_id AND NOT r.paused
		                           AND r.last_contact_at > now() - interval '30 seconds'))
		     ORDER BY (a.runner_id IS NOT NULL AND a.runner_id = $1) DESC, t.id
		     FOR UPDATE OF t SKIP LOCKED LIMIT 1)
		 RETURNING id, kind, repo, payload, attempts`,
		runnerID, kinds, affinityWait.Seconds(), lease.Milliseconds()).
		Scan(&t.ID, &t.Kind, &t.Repo, &t.Payload, &t.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &t, err
}

// ErrTaskGone: the task is not running on this runner any more (its lease
// ran out and another runner took it, or it already finished).
var ErrTaskGone = errors.New("task is not running on this runner")

// FinishRunnerTask records a task's outcome. Success remembers the runner
// for the repo's mirror; a failure is retried with backoff up to
// maxTaskAttempts. final reports whether the task is now done or failed for
// good (the caller then records the failure where it belongs).
func (s *Store) FinishRunnerTask(ctx context.Context, id int64, runnerID, failure string) (t *TaskRow, final bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback(ctx)
	t = &TaskRow{}
	var affinity string
	err = tx.QueryRow(ctx,
		`SELECT id, kind, repo, payload, attempts, affinity_key FROM runner_tasks
		 WHERE id = $1 AND state = 'running' AND runner_id = $2 FOR UPDATE`, id, runnerID).
		Scan(&t.ID, &t.Kind, &t.Repo, &t.Payload, &t.Attempts, &affinity)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, ErrTaskGone
	}
	if err != nil {
		return nil, false, err
	}
	switch {
	case failure == "":
		final = true
		if _, err := tx.Exec(ctx,
			`UPDATE runner_tasks SET state = 'done', finished_at = now(), lease_until = NULL, error = '' WHERE id = $1`, id); err != nil {
			return nil, false, err
		}
		if affinity != "" {
			if _, err := tx.Exec(ctx,
				`INSERT INTO runner_affinity (key, runner_id) VALUES ($1, $2)
				 ON CONFLICT (key) DO UPDATE SET runner_id = EXCLUDED.runner_id, updated_at = now()`, affinity, runnerID); err != nil {
				return nil, false, err
			}
		}
	case t.Attempts >= maxTaskAttempts:
		final = true
		if _, err := tx.Exec(ctx,
			`UPDATE runner_tasks SET state = 'failed', finished_at = now(), lease_until = NULL, error = $2 WHERE id = $1`,
			id, truncate(failure, 300)); err != nil {
			return nil, false, err
		}
	default:
		if _, err := tx.Exec(ctx,
			`UPDATE runner_tasks SET state = 'pending', lease_until = NULL, runner_id = '', error = $2,
			     not_before = now() + $3 * interval '1 second' WHERE id = $1`,
			id, truncate(failure, 300), 30*t.Attempts); err != nil {
			return nil, false, err
		}
	}
	return t, final, tx.Commit(ctx)
}

// FailRunnerTask fails a claimed task for good (it can never succeed, e.g.
// the repo was disconnected).
func (s *Store) FailRunnerTask(ctx context.Context, id int64, reason string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE runner_tasks SET state = 'failed', finished_at = now(), lease_until = NULL, error = $2 WHERE id = $1`,
		id, truncate(reason, 300))
	return err
}

// TaskCloneURL is the clone URL with a fresh credential embedded (minted at
// claim time, so a GitHub App token is valid for the whole task) and the
// values to redact. ok=false when the repo is not connected or cannot be
// fetched with plain git.
func (s *Store) TaskCloneURL(ctx context.Context, repo string) (cloneURL string, redact []string, ok bool, err error) {
	cloneURL, token, conn, err := s.cloneTarget(ctx, repo)
	if err != nil || cloneURL == "" || conn.Provider == "codecommit" {
		return "", nil, false, err
	}
	if token != "" {
		redact = []string{token}
	}
	return cloneURL, redact, true, nil
}

// DiffKeyFromTask rebuilds a diff task's key.
func DiffKeyFromTask(t *TaskRow) (DiffKey, proto.DiffTask, error) {
	var d proto.DiffTask
	if err := json.Unmarshal(t.Payload, &d); err != nil {
		return DiffKey{}, d, fmt.Errorf("task %s: %w", strconv.FormatInt(t.ID, 10), err)
	}
	return DiffKey{Repo: t.Repo, From: d.From, To: d.To, Mode: d.Mode}, d, nil
}

// ClaimedTask returns a task running on runnerID (ErrTaskGone otherwise).
func (s *Store) ClaimedTask(ctx context.Context, id int64, runnerID string) (*TaskRow, error) {
	var t TaskRow
	err := s.pool.QueryRow(ctx,
		`SELECT id, kind, repo, payload, attempts FROM runner_tasks WHERE id = $1 AND state = 'running' AND runner_id = $2`,
		id, runnerID).Scan(&t.ID, &t.Kind, &t.Repo, &t.Payload, &t.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrTaskGone
	}
	return &t, err
}

// CodeOwnersInput is what the code owners rule knows about a change's diff.
type CodeOwnersInput struct {
	State      string   // pending | ready | failed
	Error      string   // why the diff failed
	PathsKnown bool     // false: stored before phase F
	OwnersRead bool     // false: computed by a runner too old to read CODEOWNERS
	Paths      []string // touched paths
	File       string   // CODEOWNERS path on the target ("" = none)
	Content    string
}

// ChangeCodeOwners returns what the change diff (base → head) says about
// code owners, asking for the diff when there is none yet, or when it was
// stored before paths were recorded. refs are the refs a runner fetches.
func (s *Store) ChangeCodeOwners(ctx context.Context, repo, base, head string, refs []string) (*CodeOwnersInput, error) {
	k := DiffKey{Repo: repo, From: base, To: head, Mode: "change"}
	in := &CodeOwnersInput{}
	var paths []string
	err := s.pool.QueryRow(ctx,
		`SELECT state, error, paths, owners_read, codeowners_path, codeowners FROM change_diffs
		 WHERE repo = $1 AND from_sha = $2 AND to_sha = $3 AND mode = 'change'`, repo, base, head).
		Scan(&in.State, &in.Error, &paths, &in.OwnersRead, &in.File, &in.Content)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if _, err := s.RequestDiff(ctx, k, refs, false); err != nil {
			return nil, err
		}
		return &CodeOwnersInput{State: "pending"}, nil
	case err != nil:
		return nil, err
	}
	in.PathsKnown, in.Paths = paths != nil, paths
	if in.State == "ready" && paths == nil {
		// Stored before phase F: compute it again, once, to learn its paths.
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return nil, err
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx,
			`UPDATE change_diffs SET state = 'failed', error = 'recomputing for code owners'
			 WHERE repo = $1 AND from_sha = $2 AND to_sha = $3 AND mode = 'change' AND paths IS NULL`, repo, base, head); err != nil {
			return nil, err
		}
		if err := requestDiffTx(ctx, tx, k, refs, true); err != nil {
			return nil, err
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return &CodeOwnersInput{State: "pending"}, nil
	}
	return in, nil
}

// ---- task results ----

// SetRunnerTaskResult stores a running task's answer (before it is
// finished, so a finished task always has its result).
func (s *Store) SetRunnerTaskResult(ctx context.Context, id int64, runnerID string, result any) error {
	b, err := json.Marshal(result)
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE runner_tasks SET result = $3 WHERE id = $1 AND state = 'running' AND runner_id = $2`, id, runnerID, b)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrTaskGone
	}
	return err
}

// TaskStatus is a task as its requester sees it.
type TaskStatus struct {
	ID       int64
	State    string // pending | running | done | failed
	Error    string
	Result   json.RawMessage
	QueuedAt time.Time
	Started  bool
}

// EnqueueRunnerTask queues a task once per dedupe key (a repeat is a no-op)
// on the repo's mirror, and returns its status.
func (s *Store) EnqueueRunnerTask(ctx context.Context, kind, repo, dedupe string, payload any) (*TaskStatus, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO runner_tasks (kind, repo, affinity_key, dedupe_key, payload) VALUES ($1,$2,$3,$4,$5)
		 ON CONFLICT (dedupe_key) DO NOTHING`, kind, repo, mirrorAffinity(repo), dedupe, b); err != nil {
		return nil, err
	}
	return s.RunnerTaskStatus(ctx, dedupe)
}

// RunnerTaskStatus returns the task with dedupe key (ErrNotFound if none).
func (s *Store) RunnerTaskStatus(ctx context.Context, dedupe string) (*TaskStatus, error) {
	var t TaskStatus
	err := s.pool.QueryRow(ctx,
		`SELECT id, state, error, COALESCE(result, 'null'::jsonb), queued_at, started_at IS NOT NULL
		 FROM runner_tasks WHERE dedupe_key = $1`, dedupe).
		Scan(&t.ID, &t.State, &t.Error, &t.Result, &t.QueuedAt, &t.Started)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &t, err
}

// MergeTaskFromRow decodes a claimed merge task's payload.
func MergeTaskFromRow(t *TaskRow) (proto.MergeTask, error) {
	var m proto.MergeTask
	err := json.Unmarshal(t.Payload, &m)
	return m, err
}
