package store

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/priytamjeepandey/forge-ci/internal/live"
	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

// Forge Live operations (phase B): what Forge asks an agent to do. A page
// creates one; the target's agent claims it by long-polling, runs it and
// reports. One sync or rollback per application at a time.

// ErrOpBusy: the app already has a sync or rollback waiting or running.
var ErrOpBusy = errors.New("another sync or rollback of this application is in progress")

// LiveOp is one operation and how it went.
type LiveOp struct {
	ID          int64                       `json:"id"`
	AppID       int64                       `json:"app_id"`
	TargetID    int64                       `json:"target_id"`
	Kind        string                      `json:"kind"`
	State       string                      `json:"state"`
	Params      live.Op                     `json:"params"`
	Result      *live.OpResult              `json:"result"`
	Message     string                      `json:"message"`
	Revision    string                      `json:"revision"`
	RequestedBy string                      `json:"requested_by"`
	CreatedAt   time.Time                   `json:"created_at"`
	StartedAt   *time.Time                  `json:"started_at"`
	FinishedAt  *time.Time                  `json:"finished_at"`
	Approvals   []LiveOpApproval            `json:"approvals"`
	Rule        *proto.ProtectedEnvironment `json:"rule,omitempty"` // when waiting for approval
}

// LiveOpApproval is one vote on an operation waiting for approval.
type LiveOpApproval struct {
	Approver  string    `json:"approver"`
	Verdict   string    `json:"verdict"`
	Comment   string    `json:"comment"`
	CreatedAt time.Time `json:"created_at"`
}

const liveOpCols = `id, app_id, target_id, kind, state, params, result, message, revision, requested_by, created_at, started_at, finished_at`

func scanLiveOp(row pgx.Row) (*LiveOp, error) {
	var o LiveOp
	var params, result []byte
	if err := row.Scan(&o.ID, &o.AppID, &o.TargetID, &o.Kind, &o.State, &params, &result, &o.Message, &o.Revision,
		&o.RequestedBy, &o.CreatedAt, &o.StartedAt, &o.FinishedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	_ = json.Unmarshal(params, &o.Params)
	if result != nil {
		o.Result = &live.OpResult{}
		_ = json.Unmarshal(result, o.Result)
	}
	o.Approvals = []LiveOpApproval{}
	return &o, nil
}

// opsChannel wakes agents waiting for operations; the payload is the target id.
const opsChannel = "forge_live_ops"

// CreateLiveOp queues an operation for app. Syncs and rollbacks to a
// protected environment (one with required approvals) wait in 'approval'
// unless they are dry runs; a frozen environment refuses them.
func (s *Store) CreateLiveOp(ctx context.Context, appID int64, p live.Op, by string) (*LiveOp, error) {
	a, err := s.LiveApp(ctx, appID)
	if err != nil {
		return nil, err
	}
	state := "pending"
	changes := p.Kind == live.OpSync || p.Kind == live.OpRollback
	if changes && !p.DryRun && a.Environment != "" {
		if frozen, err := s.IsFrozen(ctx, a.Repo, a.Environment); err != nil {
			return nil, err
		} else if frozen {
			return nil, ErrFrozen
		}
		rule, err := s.resolveProtectedEnv(ctx, a.Repo, a.Environment)
		if err != nil {
			return nil, err
		}
		if rule != nil && rule.RequiredApprovals > 0 {
			state = "approval"
		}
	}
	p.App = live.App{Key: a.Key, Name: a.Name, Namespace: a.Namespace, Group: a.Group}
	params, _ := json.Marshal(p)
	o, err := scanLiveOp(s.pool.QueryRow(ctx, `INSERT INTO live_ops (app_id, target_id, kind, state, params, revision, requested_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING `+liveOpCols, appID, a.TargetID, p.Kind, state, params, p.Revision, by))
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return nil, ErrOpBusy
	}
	if err != nil {
		return nil, err
	}
	return o, s.notifyOp(ctx, o)
}

// ErrFrozen: deploys to the environment are frozen right now.
var ErrFrozen = errors.New("deploys to this environment are frozen")

func (s *Store) notifyOp(ctx context.Context, o *LiveOp) error {
	if _, err := s.pool.Exec(ctx, `SELECT pg_notify($1, $2)`, opsChannel, strconv.FormatInt(o.TargetID, 10)); err != nil {
		return err
	}
	b, _ := json.Marshal(liveNote{ID: o.AppID, Op: o.ID})
	_, err := s.pool.Exec(ctx, `SELECT pg_notify($1, $2)`, liveChannel, string(b))
	return err
}

// LiveOp returns an operation with its approvals (and its rule while it
// waits for them).
func (s *Store) LiveOp(ctx context.Context, id int64) (*LiveOp, error) {
	o, err := scanLiveOp(s.pool.QueryRow(ctx, `SELECT `+liveOpCols+` FROM live_ops WHERE id=$1`, id))
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT approver, verdict, comment, created_at FROM live_op_approvals WHERE op_id=$1 ORDER BY created_at`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var v LiveOpApproval
		if err := rows.Scan(&v.Approver, &v.Verdict, &v.Comment, &v.CreatedAt); err != nil {
			return nil, err
		}
		o.Approvals = append(o.Approvals, v)
	}
	if o.State == "approval" {
		if a, err := s.LiveApp(ctx, o.AppID); err == nil {
			o.Rule, _ = s.resolveProtectedEnv(ctx, a.Repo, a.Environment)
		}
	}
	return o, rows.Err()
}

// LiveAppOps lists an application's operations, newest first.
func (s *Store) LiveAppOps(ctx context.Context, appID int64, limit int) ([]LiveOp, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+liveOpCols+` FROM live_ops WHERE app_id=$1 AND kind <> 'logs' ORDER BY id DESC LIMIT $2`, appID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LiveOp{}
	for rows.Next() {
		o, err := scanLiveOp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *o)
	}
	return out, rows.Err()
}

// ApproveLiveOp records a vote under the environment's rule (the same rule
// and eligibility as approving a deploy job). Enough approvals release the
// operation to the agent; one rejection cancels it.
func (s *Store) ApproveLiveOp(ctx context.Context, id int64, approver, verdict, comment string) (*LiveOp, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var state, requester, repo, env string
	var target int64
	err = tx.QueryRow(ctx, `SELECT o.state, o.requested_by, t.repo, t.environment, o.target_id
		FROM live_ops o JOIN live_apps a ON a.id = o.app_id JOIN live_targets t ON t.id = a.target_id
		WHERE o.id=$1 FOR UPDATE OF o`, id).Scan(&state, &requester, &repo, &env, &target)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if state != "approval" {
		return nil, ErrNotBlocked
	}
	rule, err := s.resolveProtectedEnv(ctx, repo, env)
	if err != nil {
		return nil, err
	}
	if rule == nil {
		rule = &proto.ProtectedEnvironment{RequiredApprovals: 1, AllowSelfApproval: true}
	}
	group, err := s.checkApprover(ctx, tx, repo, rule, approver, requester)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO live_op_approvals (op_id, approver, verdict, comment) VALUES ($1,$2,$3,$4)`,
		id, approver, verdict, comment); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, ErrDuplicateVote
		}
		return nil, err
	}
	next := ""
	if verdict == "rejected" {
		next = "canceled"
	} else {
		var approved, fromGroup int
		rows, err := tx.Query(ctx, `SELECT approver FROM live_op_approvals WHERE op_id=$1 AND verdict='approved'`, id)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var who string
			if err := rows.Scan(&who); err != nil {
				rows.Close()
				return nil, err
			}
			approved++
			if group[strings.ToLower(who)] {
				fromGroup++
			}
		}
		rows.Close()
		if approved >= rule.RequiredApprovals && (rule.RequiredGroup == "" || fromGroup >= rule.RequiredGroupApprovals) {
			next = "pending"
		}
	}
	if next != "" {
		msg := ""
		if next == "canceled" {
			msg = "Rejected by " + approver
		}
		if _, err := tx.Exec(ctx, `UPDATE live_ops SET state=$2, message=$3, finished_at=CASE WHEN $2='canceled' THEN now() END WHERE id=$1`,
			id, next, msg); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	o, err := s.LiveOp(ctx, id)
	if err != nil {
		return nil, err
	}
	return o, s.notifyOp(ctx, o)
}

// CancelLiveOp stops an operation that has not finished: a waiting one is
// dropped, a running one (a log stream) is marked canceled and the agent's
// next output post tells it to stop.
func (s *Store) CancelLiveOp(ctx context.Context, id int64, by string) error {
	o, err := scanLiveOp(s.pool.QueryRow(ctx, `UPDATE live_ops SET state='canceled', message=$2, finished_at=now()
		WHERE id=$1 AND state IN ('approval','pending','running') RETURNING `+liveOpCols, id, "Canceled by "+by))
	if err != nil {
		return err
	}
	return s.notifyOp(ctx, o)
}

// ClaimLiveOp hands target's oldest pending operation to its agent (nil
// when there is none).
func (s *Store) ClaimLiveOp(ctx context.Context, targetID int64) (*LiveOp, error) {
	o, err := scanLiveOp(s.pool.QueryRow(ctx, `UPDATE live_ops SET state='running', started_at=now()
		WHERE id = (SELECT id FROM live_ops WHERE target_id=$1 AND state='pending' ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED)
		RETURNING `+liveOpCols, targetID))
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	o.Params.ID = o.ID
	return o, s.notifyOp(ctx, o)
}

// maxOpOutput bounds what one operation may stream into Postgres.
const maxOpOutput = 8 << 20

// AppendLiveOpOutput stores a chunk the agent streamed. ok=false tells the
// agent to stop (the operation was canceled, or it is over its budget).
func (s *Store) AppendLiveOpOutput(ctx context.Context, targetID, id int64, chunk []byte) (ok bool, err error) {
	tag, err := s.pool.Exec(ctx, `INSERT INTO live_op_output (op_id, chunk)
		SELECT id, $3 FROM live_ops WHERE id=$1 AND target_id=$2 AND state='running'
		AND (SELECT coalesce(sum(length(chunk)), 0) FROM live_op_output WHERE op_id=$1) < $4`, id, targetID, chunk, maxOpOutput)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// LiveOpOutput returns the chunks after seq.
func (s *Store) LiveOpOutput(ctx context.Context, id, after int64) (chunks [][]byte, last int64, err error) {
	rows, err := s.pool.Query(ctx, `SELECT seq, chunk FROM live_op_output WHERE op_id=$1 AND seq > $2 ORDER BY seq LIMIT 200`, id, after)
	if err != nil {
		return nil, after, err
	}
	defer rows.Close()
	last = after
	for rows.Next() {
		var c []byte
		if err := rows.Scan(&last, &c); err != nil {
			return nil, after, err
		}
		chunks = append(chunks, c)
	}
	return chunks, last, rows.Err()
}

// FinishLiveOp records how an operation the agent ran ended.
func (s *Store) FinishLiveOp(ctx context.Context, targetID, id int64, res live.OpResult) (*LiveOp, error) {
	state := "failed"
	if res.OK {
		state = "succeeded"
	}
	b, _ := json.Marshal(res)
	o, err := scanLiveOp(s.pool.QueryRow(ctx, `UPDATE live_ops SET state=CASE WHEN state='canceled' THEN state ELSE $3 END,
		result=$4, message=CASE WHEN state='canceled' THEN message ELSE $5 END, finished_at=coalesce(finished_at, now()),
		revision=CASE WHEN $6 <> '' THEN $6 ELSE revision END
		WHERE id=$1 AND target_id=$2 AND state IN ('running','canceled') RETURNING `+liveOpCols, id, targetID, state, b, res.Message, res.Revision))
	if err != nil {
		return nil, err
	}
	return o, s.notifyOp(ctx, o)
}

// ExpireLiveOps fails operations an agent claimed and never finished (it
// restarted), and drops log output older than an hour.
func (s *Store) ExpireLiveOps(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, `UPDATE live_ops SET state='failed', message='The agent stopped before finishing', finished_at=now()
		WHERE state='running' AND started_at < now() - interval '20 minutes'`); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `DELETE FROM live_op_output WHERE created_at < now() - interval '1 hour'`)
	return err
}

// ListenLiveOps calls fn with the target id of each new operation.
func (s *Store) ListenLiveOps(ctx context.Context, fn func(targetID int64)) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN "+opsChannel); err != nil {
		return err
	}
	defer func() { _, _ = conn.Exec(context.Background(), "UNLISTEN "+opsChannel) }()
	for {
		n, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return err
		}
		if id, err := strconv.ParseInt(n.Payload, 10, 64); err == nil {
			fn(id)
		}
	}
}
