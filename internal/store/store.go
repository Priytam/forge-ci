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
	"sort"
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
	ErrForbidden     = errors.New("approver's role may not approve this environment")
	ErrSelfApproval  = errors.New("pipeline author may not approve their own deployment")
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
		`INSERT INTO pipelines (repo, ref, sha, config_yaml, triggered_by)
		 VALUES ($1,$2,$3,$4,$5) RETURNING id, created_at`,
		req.Repo, req.Ref, req.SHA, req.Config, req.TriggeredBy).Scan(&p.ID, &p.CreatedAt)
	if err != nil {
		return nil, err
	}

	ids := map[string]int64{}
	for _, j := range jobs {
		env, _ := json.Marshal(j.Env)
		artifacts, _ := json.Marshal(j.ArtifactPaths)
		tags := j.Tags
		if tags == nil {
			tags = []string{}
		}
		var image, environment *string
		if j.Image != "" {
			image = &j.Image
		}
		if j.Environment != "" {
			environment = &j.Environment
		}
		var id int64
		err = tx.QueryRow(ctx,
			`INSERT INTO jobs (pipeline_id, name, stage, stage_idx, image, script, env, environment, tags, artifact_paths)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id`,
			p.ID, j.Name, j.Stage, j.StageIdx, image, j.Script, env, environment, tags, artifacts).Scan(&id)
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

// deriveStages folds per-job statuses into one status per stage, ordered by
// stage index.
func deriveStages(stages []stageJob) []proto.StageStatus {
	type agg struct {
		name     string
		idx      int
		statuses []string
	}
	byIdx := map[int]*agg{}
	order := []int{}
	for _, sj := range stages {
		a, ok := byIdx[sj.Idx]
		if !ok {
			a = &agg{name: sj.Stage, idx: sj.Idx}
			byIdx[sj.Idx] = a
			order = append(order, sj.Idx)
		}
		a.statuses = append(a.statuses, sj.Status)
	}
	sort.Ints(order)
	out := make([]proto.StageStatus, 0, len(order))
	for _, i := range order {
		out = append(out, proto.StageStatus{Name: byIdx[i].name, Status: deriveStatus(byIdx[i].statuses)})
	}
	return out
}

type stageJob struct {
	Stage  string `json:"stage"`
	Idx    int    `json:"idx"`
	Status string `json:"status"`
}

// ListPipelines returns recent pipelines (optionally for one repo), each with
// its derived overall status and per-stage statuses.
func (s *Store) ListPipelines(ctx context.Context, repo string) ([]proto.Pipeline, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT p.id, p.repo, p.ref, p.sha, p.created_at,
		        COALESCE(json_agg(json_build_object(
		            'stage', j.stage, 'idx', j.stage_idx, 'status', j.status
		        ) ORDER BY j.stage_idx) FILTER (WHERE j.id IS NOT NULL), '[]')
		 FROM pipelines p LEFT JOIN jobs j ON j.pipeline_id = p.id
		 WHERE ($1 = '' OR p.repo = $1)
		 GROUP BY p.id ORDER BY p.id DESC LIMIT 200`, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []proto.Pipeline{}
	for rows.Next() {
		var p proto.Pipeline
		var raw []byte
		if err := rows.Scan(&p.ID, &p.Repo, &p.Ref, &p.SHA, &p.CreatedAt, &raw); err != nil {
			return nil, err
		}
		var sjs []stageJob
		if err := json.Unmarshal(raw, &sjs); err != nil {
			return nil, err
		}
		statuses := make([]string, len(sjs))
		for i, sj := range sjs {
			statuses[i] = sj.Status
		}
		p.Status = deriveStatus(statuses)
		p.Stages = deriveStages(sjs)
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListRepos aggregates pipelines into one summary card per repo, most
// recently active first.
func (s *Store) ListRepos(ctx context.Context) ([]proto.RepoSummary, error) {
	pipelines, err := s.ListPipelines(ctx, "")
	if err != nil {
		return nil, err
	}
	const recentStatusMax = 5
	byRepo := map[string]*proto.RepoSummary{}
	order := []string{}
	for i := range pipelines {
		p := pipelines[i]
		r, ok := byRepo[p.Repo]
		if !ok {
			r = &proto.RepoSummary{Repo: p.Repo, LastPipeline: &pipelines[i], LastActivityAt: p.CreatedAt}
			byRepo[p.Repo] = r
			order = append(order, p.Repo)
		}
		r.PipelineCount++
		switch p.Status {
		case "success":
			r.SuccessCount++
		case "failed":
			r.FailedCount++
		}
		if len(r.RecentStatuses) < recentStatusMax {
			r.RecentStatuses = append(r.RecentStatuses, p.Status)
		}
		seen := false
		for _, ref := range r.Refs {
			if ref == p.Ref {
				seen = true
				break
			}
		}
		if !seen {
			r.Refs = append(r.Refs, p.Ref)
		}
	}
	// pipelines are newest-first, so insertion order == last-activity order.
	out := make([]proto.RepoSummary, 0, len(order))
	for _, name := range order {
		out = append(out, *byRepo[name])
	}
	return out, nil
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
	sjs := []stageJob{}
	for rows.Next() {
		var j proto.Job
		if err := rows.Scan(&j.ID, &j.PipelineID, &j.Name, &j.Stage, &j.StageIdx, &j.Image,
			&j.Environment, &j.Status, &j.StartedAt, &j.FinishedAt, &j.ExitCode, &j.Needs); err != nil {
			return nil, nil, err
		}
		jobs = append(jobs, j)
		statuses = append(statuses, j.Status)
		sjs = append(sjs, stageJob{Stage: j.Stage, Idx: j.StageIdx, Status: j.Status})
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	p.Status = deriveStatus(statuses)
	p.Stages = deriveStages(sjs)
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

	var status, repo, triggeredBy string
	var environment *string
	err = tx.QueryRow(ctx,
		`SELECT j.status, j.environment, p.repo, p.triggered_by
		 FROM jobs j JOIN pipelines p ON p.id = j.pipeline_id
		 WHERE j.id=$1 FOR UPDATE OF j`, jobID).
		Scan(&status, &environment, &repo, &triggeredBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if status != "blocked" || environment == nil {
		return nil, ErrNotBlocked
	}

	rule, err := s.resolveProtectedEnv(ctx, repo, *environment)
	if err != nil {
		return nil, err
	}
	if rule == nil {
		return nil, ErrNotBlocked
	}

	// RBAC: enforced when the repo has members; a repo with no members runs
	// in bootstrap mode (anyone may approve) — see docs/rbac-approvals.md.
	hasMembers, err := s.repoHasMembers(ctx, repo)
	if err != nil {
		return nil, err
	}
	if hasMembers {
		role, err := s.memberRole(ctx, repo, req.Approver)
		if err != nil {
			return nil, err
		}
		allowed := false
		for _, r := range rule.ApproverRoles {
			if role == r {
				allowed = true
				break
			}
		}
		if !allowed {
			return nil, ErrForbidden
		}
	}
	if !rule.AllowSelfApproval && triggeredBy != "" && req.Approver == triggeredBy {
		return nil, ErrSelfApproval
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
		var approved int
		err = tx.QueryRow(ctx,
			`SELECT count(*) FROM job_approvals WHERE job_id=$1 AND verdict='approved'`,
			jobID).Scan(&approved)
		if err != nil {
			return nil, err
		}
		if approved >= rule.RequiredApprovals {
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

// AcquireJob atomically claims the oldest pending job for a runner and
// resolves the env to inject: repo-level CI/CD variables (respecting
// protected refs and environment scope) overlaid by job-level YAML variables.
// Returns (nil, nil) when the queue is empty.
func (s *Store) AcquireJob(ctx context.Context, req proto.AcquireRequest) (*proto.RunnerJob, error) {
	paused, err := s.TouchRunner(ctx, req.RunnerID, req.Executor, req.Tags)
	if err != nil {
		return nil, err
	}
	if paused {
		return nil, nil
	}
	runnerTags := req.Tags
	if runnerTags == nil {
		runnerTags = []string{}
	}
	var j proto.RunnerJob
	var image, environment *string
	var envRaw, artifactsRaw []byte
	var repo, ref string
	err = s.pool.QueryRow(ctx,
		`UPDATE jobs j
		 SET status='running', started_at=now(), heartbeat_at=now(), runner_id=$1
		 FROM (SELECT id FROM jobs
		       WHERE status='pending' AND tags <@ $2::text[]
		       ORDER BY id
		       FOR UPDATE SKIP LOCKED LIMIT 1) next,
		      pipelines p
		 WHERE j.id = next.id AND p.id = j.pipeline_id
		 RETURNING j.id, j.pipeline_id, j.name, j.image, j.script, j.env,
		           j.artifact_paths, j.environment, p.repo, p.ref`,
		req.RunnerID, runnerTags).
		Scan(&j.ID, &j.PipelineID, &j.Name, &image, &j.Script, &envRaw,
			&artifactsRaw, &environment, &repo, &ref)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if image != nil {
		j.Image = *image
	}
	env := ""
	if environment != nil {
		env = *environment
	}
	resolved, err := s.ResolveVariables(ctx, repo, ref, env)
	if err != nil {
		return nil, err
	}
	jobEnv := map[string]string{}
	_ = json.Unmarshal(envRaw, &jobEnv)
	for k, v := range jobEnv { // job-level YAML variables win
		resolved[k] = v
	}
	j.Env = resolved
	j.ArtifactPaths = []string{}
	_ = json.Unmarshal(artifactsRaw, &j.ArtifactPaths)
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
// protectedFor matches a job's environment against a repo-specific rule or
// the global (repo='') default.
const protectedFor = `EXISTS (
	SELECT 1 FROM protected_environments pe, pipelines p
	WHERE p.id = j.pipeline_id AND pe.name = j.environment AND pe.repo IN ('', p.repo))`

func (s *Store) PromoteReadyJobs(ctx context.Context) (int64, error) {
	blocked, err := s.pool.Exec(ctx,
		`UPDATE jobs j SET status='blocked', blocked_at=now()
		 WHERE j.status='created' AND `+protectedFor+` AND NOT `+needsUnmet)
	if err != nil {
		return 0, err
	}
	pending, err := s.pool.Exec(ctx,
		`UPDATE jobs j SET status='pending'
		 WHERE j.status='created' AND NOT `+protectedFor+` AND NOT `+needsUnmet)
	if err != nil {
		return 0, err
	}
	return blocked.RowsAffected() + pending.RowsAffected(), nil
}

// ExpireBlockedJobs fails blocked jobs whose approval window has lapsed,
// using the repo-specific timeout when one exists.
func (s *Store) ExpireBlockedJobs(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE jobs j SET status='failed', finished_at=now()
		 WHERE j.status='blocked'
		   AND j.blocked_at + make_interval(hours => (
		       SELECT pe.approval_timeout_hours
		       FROM protected_environments pe, pipelines p
		       WHERE p.id = j.pipeline_id AND pe.name = j.environment
		         AND pe.repo IN ('', p.repo)
		       ORDER BY pe.repo DESC LIMIT 1)) < now()`)
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
