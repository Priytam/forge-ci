// Package store is the single Postgres-backed source of truth. All job state
// transitions are transactional; the pending-job queue is claimed with
// FOR UPDATE SKIP LOCKED so concurrent schedulers/runners are safe.
package store

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/priytamjeepandey/forge-ci/internal/compiler"
	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

//go:embed migrations.sql
var migrations string

var (
	ErrNotFound      = errors.New("not found")
	ErrNotBlocked    = errors.New("job is not waiting for approval")
	ErrDuplicateVote = errors.New("approver has already voted on this job")
)

type Store struct{ pool *pgxpool.Pool }

func New(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("postgres ping: %w", err)
	}
	if _, err := pool.Exec(ctx, migrations); err != nil {
		return nil, fmt.Errorf("migrations: %w", err)
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() { s.pool.Close() }

// ---- pipelines ----

func (s *Store) CreatePipeline(ctx context.Context, req proto.CreatePipelineRequest, jobs []compiler.CompiledJob) (*proto.Pipeline, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var p proto.Pipeline
	p.Repo, p.Ref, p.SHA = req.Repo, req.Ref, req.SHA
	p.Status = "created"
	err = tx.QueryRow(ctx,
		`INSERT INTO pipelines (repo, ref, sha, config_yaml) VALUES ($1,$2,$3,$4)
		 RETURNING id, created_at`,
		req.Repo, req.Ref, req.SHA, req.Config).Scan(&p.ID, &p.CreatedAt)
	if err != nil {
		return nil, err
	}

	ids := map[string]int64{}
	for _, j := range jobs {
		env, _ := json.Marshal(j.Env)
		var image, environment *string
		if j.Image != "" {
			image = &j.Image
		}
		if j.Environment != "" {
			environment = &j.Environment
		}
		var id int64
		err = tx.QueryRow(ctx,
			`INSERT INTO jobs (pipeline_id, name, stage, stage_idx, image, script, env, environment)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id`,
			p.ID, j.Name, j.Stage, j.StageIdx, image, j.Script, env, environment).Scan(&id)
		if err != nil {
			return nil, err
		}
		ids[j.Name] = id
	}
	for _, j := range jobs {
		for _, dep := range j.Needs {
			if _, err := tx.Exec(ctx,
				`INSERT INTO job_needs (job_id, needs_job_id) VALUES ($1,$2)`,
				ids[j.Name], ids[dep]); err != nil {
				return nil, err
			}
		}
	}
	return &p, tx.Commit(ctx)
}

// derive overall pipeline status from its jobs' statuses.
func deriveStatus(statuses []string) string {
	has := map[string]bool{}
	for _, st := range statuses {
		has[st] = true
	}
	switch {
	case len(statuses) == 0:
		return "created"
	case has["failed"]:
		return "failed"
	case has["canceled"]:
		return "canceled"
	case has["running"] || has["pending"]:
		return "running"
	case has["blocked"]:
		return "blocked"
	case has["created"]:
		return "running"
	default:
		return "success"
	}
}

func (s *Store) ListPipelines(ctx context.Context) ([]proto.Pipeline, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT p.id, p.repo, p.ref, p.sha, p.created_at,
		        COALESCE(array_agg(j.status) FILTER (WHERE j.id IS NOT NULL), '{}')
		 FROM pipelines p LEFT JOIN jobs j ON j.pipeline_id = p.id
		 GROUP BY p.id ORDER BY p.id DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []proto.Pipeline{}
	for rows.Next() {
		var p proto.Pipeline
		var statuses []string
		if err := rows.Scan(&p.ID, &p.Repo, &p.Ref, &p.SHA, &p.CreatedAt, &statuses); err != nil {
			return nil, err
		}
		p.Status = deriveStatus(statuses)
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) GetPipeline(ctx context.Context, id int64) (*proto.Pipeline, []proto.Job, error) {
	var p proto.Pipeline
	err := s.pool.QueryRow(ctx,
		`SELECT id, repo, ref, sha, created_at FROM pipelines WHERE id=$1`, id).
		Scan(&p.ID, &p.Repo, &p.Ref, &p.SHA, &p.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}

	rows, err := s.pool.Query(ctx,
		`SELECT j.id, j.pipeline_id, j.name, j.stage, j.stage_idx, j.image, j.environment,
		        j.status, j.started_at, j.finished_at, j.exit_code,
		        COALESCE(array_agg(n.needs_job_id) FILTER (WHERE n.needs_job_id IS NOT NULL), '{}')
		 FROM jobs j LEFT JOIN job_needs n ON n.job_id = j.id
		 WHERE j.pipeline_id = $1
		 GROUP BY j.id ORDER BY j.stage_idx, j.name`, id)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	jobs := []proto.Job{}
	statuses := []string{}
	for rows.Next() {
		var j proto.Job
		if err := rows.Scan(&j.ID, &j.PipelineID, &j.Name, &j.Stage, &j.StageIdx, &j.Image,
			&j.Environment, &j.Status, &j.StartedAt, &j.FinishedAt, &j.ExitCode, &j.Needs); err != nil {
			return nil, nil, err
		}
		jobs = append(jobs, j)
		statuses = append(statuses, j.Status)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	p.Status = deriveStatus(statuses)
	return &p, jobs, nil
}

func (s *Store) GetJob(ctx context.Context, id int64) (*proto.Job, error) {
	var j proto.Job
	err := s.pool.QueryRow(ctx,
		`SELECT j.id, j.pipeline_id, j.name, j.stage, j.stage_idx, j.image, j.environment,
		        j.status, j.started_at, j.finished_at, j.exit_code,
		        COALESCE(array_agg(n.needs_job_id) FILTER (WHERE n.needs_job_id IS NOT NULL), '{}')
		 FROM jobs j LEFT JOIN job_needs n ON n.job_id = j.id
		 WHERE j.id = $1 GROUP BY j.id`, id).
		Scan(&j.ID, &j.PipelineID, &j.Name, &j.Stage, &j.StageIdx, &j.Image,
			&j.Environment, &j.Status, &j.StartedAt, &j.FinishedAt, &j.ExitCode, &j.Needs)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &j, err
}

// ---- logs ----

func (s *Store) AppendLog(ctx context.Context, jobID int64, chunk string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO job_logs (job_id, chunk) VALUES ($1,$2)`, jobID, chunk)
	return err
}

func (s *Store) GetLogs(ctx context.Context, jobID int64) (string, error) {
	var logs string
	err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(string_agg(chunk, '' ORDER BY id), '') FROM job_logs WHERE job_id=$1`,
		jobID).Scan(&logs)
	return logs, err
}

// ---- approvals ----

// Approve records a vote and, inside the same transaction, releases the job
// (blocked -> pending) once required approvals are met, or fails it on reject.
func (s *Store) Approve(ctx context.Context, jobID int64, req proto.ApprovalRequest) (*proto.Job, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var status string
	var environment *string
	err = tx.QueryRow(ctx,
		`SELECT status, environment FROM jobs WHERE id=$1 FOR UPDATE`, jobID).
		Scan(&status, &environment)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if status != "blocked" || environment == nil {
		return nil, ErrNotBlocked
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO job_approvals (job_id, approver, verdict, comment) VALUES ($1,$2,$3,$4)`,
		jobID, req.Approver, req.Verdict, req.Comment); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, ErrDuplicateVote
		}
		return nil, err
	}

	if req.Verdict == "rejected" {
		if _, err := tx.Exec(ctx,
			`UPDATE jobs SET status='failed', finished_at=now() WHERE id=$1`, jobID); err != nil {
			return nil, err
		}
	} else {
		var approved, required int
		err = tx.QueryRow(ctx,
			`SELECT (SELECT count(*) FROM job_approvals WHERE job_id=$1 AND verdict='approved'),
			        (SELECT required_approvals FROM protected_environments WHERE name=$2)`,
			jobID, *environment).Scan(&approved, &required)
		if err != nil {
			return nil, err
		}
		if approved >= required {
			if _, err := tx.Exec(ctx,
				`UPDATE jobs SET status='pending', blocked_at=NULL WHERE id=$1`, jobID); err != nil {
				return nil, err
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.GetJob(ctx, jobID)
}

// ---- runner queue ----

// AcquireJob atomically claims the oldest pending job for a runner.
// Returns (nil, nil) when the queue is empty.
func (s *Store) AcquireJob(ctx context.Context, runnerID string) (*proto.RunnerJob, error) {
	var j proto.RunnerJob
	var image *string
	var envRaw []byte
	err := s.pool.QueryRow(ctx,
		`UPDATE jobs SET status='running', started_at=now(), heartbeat_at=now(), runner_id=$1
		 WHERE id = (SELECT id FROM jobs WHERE status='pending' ORDER BY id
		             FOR UPDATE SKIP LOCKED LIMIT 1)
		 RETURNING id, pipeline_id, name, image, script, env`, runnerID).
		Scan(&j.ID, &j.PipelineID, &j.Name, &image, &j.Script, &envRaw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if image != nil {
		j.Image = *image
	}
	j.Env = map[string]string{}
	_ = json.Unmarshal(envRaw, &j.Env)
	return &j, nil
}

func (s *Store) Heartbeat(ctx context.Context, jobID int64) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE jobs SET heartbeat_at=now() WHERE id=$1 AND status='running'`, jobID)
	return err
}

func (s *Store) CompleteJob(ctx context.Context, jobID int64, status string, exitCode int) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE jobs SET status=$2, exit_code=$3, finished_at=now()
		 WHERE id=$1 AND status='running'`, jobID, status, exitCode)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- scheduler transitions (each idempotent; called every tick) ----

const needsUnmet = `EXISTS (
	SELECT 1 FROM job_needs n JOIN jobs d ON d.id = n.needs_job_id
	WHERE n.job_id = j.id AND d.status <> 'success')`

// CancelDeadJobs cancels created jobs whose dependencies failed or were canceled.
func (s *Store) CancelDeadJobs(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE jobs j SET status='canceled', finished_at=now()
		 WHERE j.status='created' AND EXISTS (
		   SELECT 1 FROM job_needs n JOIN jobs d ON d.id = n.needs_job_id
		   WHERE n.job_id = j.id AND d.status IN ('failed','canceled'))`)
	return tag.RowsAffected(), err
}

// PromoteReadyJobs moves created jobs with all needs satisfied to pending, or
// to blocked when they target a protected environment.
func (s *Store) PromoteReadyJobs(ctx context.Context) (int64, error) {
	blocked, err := s.pool.Exec(ctx,
		`UPDATE jobs j SET status='blocked', blocked_at=now()
		 WHERE j.status='created'
		   AND EXISTS (SELECT 1 FROM protected_environments pe WHERE pe.name = j.environment)
		   AND NOT `+needsUnmet)
	if err != nil {
		return 0, err
	}
	pending, err := s.pool.Exec(ctx,
		`UPDATE jobs j SET status='pending'
		 WHERE j.status='created'
		   AND NOT EXISTS (SELECT 1 FROM protected_environments pe WHERE pe.name = j.environment)
		   AND NOT `+needsUnmet)
	if err != nil {
		return 0, err
	}
	return blocked.RowsAffected() + pending.RowsAffected(), nil
}

// ExpireBlockedJobs fails blocked jobs whose approval window has lapsed.
func (s *Store) ExpireBlockedJobs(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE jobs j SET status='failed', finished_at=now()
		 FROM protected_environments pe
		 WHERE j.status='blocked' AND pe.name = j.environment
		   AND j.blocked_at + make_interval(hours => pe.approval_timeout_hours) < now()`)
	return tag.RowsAffected(), err
}

// FailStaleJobs fails running jobs whose runner stopped heartbeating.
func (s *Store) FailStaleJobs(ctx context.Context, staleAfter time.Duration) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE jobs SET status='failed', finished_at=now()
		 WHERE status='running' AND heartbeat_at < now() - $1::interval`,
		staleAfter.String())
	return tag.RowsAffected(), err
}
