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
	"log/slog"
	"os"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/priytamjeepandey/forge-ci/internal/compiler"
	"github.com/priytamjeepandey/forge-ci/internal/proto"
	"github.com/priytamjeepandey/forge-ci/internal/secret"
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

type Store struct {
	pool   *pgxpool.Pool
	cipher *secret.Cipher // envelope encryption for secrets at rest

	// Execution-timeout policy (env-configured, see New).
	defaultJobTimeout time.Duration // DEFAULT_JOB_TIMEOUT, jobs without timeout:
	maxJobTimeout     time.Duration // MAX_JOB_TIMEOUT, hard cap on job values
	queueTimeout      time.Duration // QUEUE_TIMEOUT, max time in pending
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return fallback
}

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
	cipher, err := secret.FromEnv()
	if err != nil {
		return nil, fmt.Errorf("secret cipher: %w", err)
	}
	s := &Store{
		pool:              pool,
		cipher:            cipher,
		defaultJobTimeout: envDuration("DEFAULT_JOB_TIMEOUT", time.Hour),
		maxJobTimeout:     envDuration("MAX_JOB_TIMEOUT", 4*time.Hour),
		queueTimeout:      envDuration("QUEUE_TIMEOUT", 24*time.Hour),
	}
	if err := s.initSecrets(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// initSecrets either re-encrypts any plaintext secret rows (when a key is
// configured) or warns loudly that secrets are stored in the clear (when it is
// not). Encryption is idempotent: already-encrypted rows are skipped.
func (s *Store) initSecrets(ctx context.Context) error {
	if s.cipher.HasKey() {
		n, err := s.MigrateSecrets(ctx)
		if err != nil {
			return fmt.Errorf("re-encrypt secrets: %w", err)
		}
		if n > 0 {
			slog.Info("secrets: re-encrypted plaintext rows at rest", "rows", n)
		}
		return nil
	}
	plaintext, err := s.countPlaintextSecrets(ctx)
	if err != nil {
		return err
	}
	if plaintext > 0 {
		slog.Warn("SECURITY: FORGE_SECRET_KEY is not set — "+
			"CI/CD variables, VCS tokens and SSO client secrets are stored in PLAINTEXT. "+
			"Set FORGE_SECRET_KEY (base64 32 bytes) to enable encryption at rest.",
			"plaintext_secret_rows", plaintext)
	}
	return nil
}

// countPlaintextSecrets counts non-empty, unencrypted secret values across the
// three secret-bearing tables.
func (s *Store) countPlaintextSecrets(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM repo_variables WHERE value <> '' AND value NOT LIKE 'enc:v1:%') +
		  (SELECT count(*) FROM repo_registry  WHERE token <> '' AND token NOT LIKE 'enc:v1:%') +
		  (SELECT count(*) FROM sso_providers  WHERE client_secret <> '' AND client_secret NOT LIKE 'enc:v1:%')`).
		Scan(&n)
	return n, err
}

// MigrateSecrets re-encrypts every plaintext secret value in place. It is a
// no-op in passthrough mode and idempotent (enc:v1: rows are skipped by the
// WHERE clause), so it is safe to run on every start.
func (s *Store) MigrateSecrets(ctx context.Context) (int, error) {
	if !s.cipher.HasKey() {
		return 0, nil
	}
	total := 0
	type target struct {
		table, keyCol, valCol string
	}
	for _, t := range []target{
		{"repo_variables", "id", "value"},
		{"repo_registry", "repo", "token"},
		{"sso_providers", "provider", "client_secret"},
	} {
		rows, err := s.pool.Query(ctx, fmt.Sprintf(
			`SELECT %s, %s FROM %s WHERE %s <> '' AND %s NOT LIKE 'enc:v1:%%'`,
			t.keyCol, t.valCol, t.table, t.valCol, t.valCol))
		if err != nil {
			return total, err
		}
		type row struct {
			key any
			val string
		}
		var pending []row
		for rows.Next() {
			var k any
			var v string
			if err := rows.Scan(&k, &v); err != nil {
				rows.Close()
				return total, err
			}
			pending = append(pending, row{k, v})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return total, err
		}
		for _, p := range pending {
			enc, err := s.cipher.Encrypt(p.val)
			if err != nil {
				return total, err
			}
			if _, err := s.pool.Exec(ctx, fmt.Sprintf(
				`UPDATE %s SET %s=$1 WHERE %s=$2`, t.table, t.valCol, t.keyCol),
				enc, p.key); err != nil {
				return total, err
			}
			total++
		}
	}
	return total, nil
}

func (s *Store) Close() { s.pool.Close() }

// ---- pipelines ----

// CreatePipeline records the run. configVersion links it to the registered
// config version it was compiled from (nil = one-off custom config).
func (s *Store) CreatePipeline(ctx context.Context, req proto.CreatePipelineRequest, jobs []compiler.CompiledJob, configVersion *int) (*proto.Pipeline, error) {
	// Repo-level runner-group selection: jobs without explicit tags inherit
	// the repo's default runner tags (job-level tags: overrides).
	defaultTags, err := s.GetRepoDefaultTags(ctx, req.Repo)
	if err != nil {
		return nil, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var p proto.Pipeline
	p.Repo, p.Ref, p.SHA = req.Repo, req.Ref, req.SHA
	p.Status = "created"
	p.ConfigVersion = configVersion
	err = tx.QueryRow(ctx,
		`INSERT INTO pipelines (repo, ref, sha, config_yaml, triggered_by, config_version)
		 VALUES ($1,$2,$3,$4,$5,$6) RETURNING id, created_at`,
		req.Repo, req.Ref, req.SHA, req.Config, req.TriggeredBy, configVersion).Scan(&p.ID, &p.CreatedAt)
	if err != nil {
		return nil, err
	}

	ids := map[string]int64{}
	for _, j := range jobs {
		env, _ := json.Marshal(j.Env)
		artifacts, _ := json.Marshal(j.ArtifactPaths)
		tags := j.Tags
		if len(tags) == 0 {
			tags = defaultTags
		}
		timeoutSec := j.TimeoutSec
		if max := int(s.maxJobTimeout.Seconds()); timeoutSec > max {
			timeoutSec = max
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
			`INSERT INTO jobs (pipeline_id, name, stage, stage_idx, image, script, env, environment, tags, artifact_paths, timeout_seconds)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING id`,
			p.ID, j.Name, j.Stage, j.StageIdx, image, j.Script, env, environment, tags, artifacts, timeoutSec).Scan(&id)
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
		`SELECT p.id, p.repo, p.ref, p.sha, p.config_version, p.created_at,
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
		if err := rows.Scan(&p.ID, &p.Repo, &p.Ref, &p.SHA, &p.ConfigVersion, &p.CreatedAt, &raw); err != nil {
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
	// Registered repos with no pipelines yet still get a card.
	registered, err := s.ListRegisteredRepos(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range registered {
		if _, seen := byRepo[r.Repo]; !seen {
			out = append(out, proto.RepoSummary{
				Repo:           r.Repo,
				Refs:           []string{},
				RecentStatuses: []string{},
				LastActivityAt: r.CreatedAt,
			})
		}
	}
	return out, nil
}

func (s *Store) GetPipeline(ctx context.Context, id int64) (*proto.Pipeline, []proto.Job, error) {
	var p proto.Pipeline
	err := s.pool.QueryRow(ctx,
		`SELECT id, repo, ref, sha, config_version, created_at FROM pipelines WHERE id=$1`, id).
		Scan(&p.ID, &p.Repo, &p.Ref, &p.SHA, &p.ConfigVersion, &p.CreatedAt)
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

// AppendLogCapped appends a log chunk while enforcing a cumulative per-job
// byte cap. capBytes <= 0 disables the cap. Once the cap is crossed it writes a
// single truncation notice, sets jobs.log_truncated, and silently drops all
// further chunks. Returns ErrNotFound if the job does not exist.
func (s *Store) AppendLogCapped(ctx context.Context, jobID int64, chunk string, capBytes int64) error {
	if capBytes <= 0 {
		return s.AppendLog(ctx, jobID, chunk)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var truncated bool
	var used int64
	err = tx.QueryRow(ctx,
		`SELECT j.log_truncated,
		        COALESCE((SELECT sum(octet_length(chunk)) FROM job_logs WHERE job_id=j.id), 0)
		 FROM jobs j WHERE j.id=$1`, jobID).Scan(&truncated, &used)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if truncated {
		return tx.Commit(ctx) // already capped — drop
	}
	if used+int64(len(chunk)) > capBytes {
		// Store the portion that still fits under the cap, then a single
		// truncation notice, and mark the job so further chunks are dropped.
		if remaining := capBytes - used; remaining > 0 {
			if _, err := tx.Exec(ctx,
				`INSERT INTO job_logs (job_id, chunk) VALUES ($1,$2)`,
				jobID, chunk[:remaining]); err != nil {
				return err
			}
		}
		notice := fmt.Sprintf(
			"\n[forge] log truncated: job exceeded MAX_JOB_LOG_BYTES (%d bytes); further output dropped\n",
			capBytes)
		if _, err := tx.Exec(ctx,
			`INSERT INTO job_logs (job_id, chunk) VALUES ($1,$2)`, jobID, notice); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE jobs SET log_truncated=TRUE WHERE id=$1`, jobID); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO job_logs (job_id, chunk) VALUES ($1,$2)`, jobID, chunk); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ---- retention GC ----

// DeleteExpiredSessions removes login sessions past their expiry.
func (s *Store) DeleteExpiredSessions(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM sessions WHERE expires_at < now()`)
	return tag.RowsAffected(), err
}

// ExpiredArtifactBlobKeys returns the blob keys (artifacts.path) of artifacts
// belonging to pipelines older than the retention window, so the caller can
// delete them from the blob store before the DB rows cascade away.
func (s *Store) ExpiredArtifactBlobKeys(ctx context.Context, olderThan time.Duration) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT a.path FROM artifacts a
		 JOIN jobs j ON j.id = a.job_id
		 JOIN pipelines p ON p.id = j.pipeline_id
		 WHERE p.created_at < now() - $1::interval`, olderThan.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// DeleteExpiredPipelines removes pipelines older than the retention window.
// Deletion cascades to jobs, job_logs, job_needs, artifacts and job_approvals.
func (s *Store) DeleteExpiredPipelines(ctx context.Context, olderThan time.Duration) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM pipelines WHERE created_at < now() - $1::interval`, olderThan.String())
	return tag.RowsAffected(), err
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
	var repo, ref, sha string
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
		           j.artifact_paths, j.environment, j.timeout_seconds,
		           p.repo, p.ref, p.sha`,
		req.RunnerID, runnerTags).
		Scan(&j.ID, &j.PipelineID, &j.Name, &image, &j.Script, &envRaw,
			&artifactsRaw, &environment, &j.TimeoutSeconds, &repo, &ref, &sha)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if image != nil {
		j.Image = *image
	}
	if j.TimeoutSeconds <= 0 {
		j.TimeoutSeconds = int(s.defaultJobTimeout.Seconds())
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

	// Source checkout info when the repo is registered against a real VCS.
	cloneURL, token, err := s.cloneAuth(ctx, repo)
	if err != nil {
		return nil, err
	}
	if cloneURL != "" {
		j.CloneURL, j.SHA, j.Ref, j.RepoName = cloneURL, sha, ref, repo
		if token != "" {
			j.RedactValues = append(j.RedactValues, token)
		}
	}

	// Artifact passing: archives uploaded by the jobs this job needs.
	deps, err := s.pool.Query(ctx,
		`SELECT a.id, d.name, a.name
		 FROM job_needs n
		 JOIN jobs d ON d.id = n.needs_job_id
		 JOIN artifacts a ON a.job_id = d.id
		 WHERE n.job_id = $1 ORDER BY a.id`, j.ID)
	if err != nil {
		return nil, err
	}
	defer deps.Close()
	for deps.Next() {
		var da proto.DependencyArtifact
		if err := deps.Scan(&da.ArtifactID, &da.JobName, &da.Name); err != nil {
			return nil, err
		}
		j.Dependencies = append(j.Dependencies, da)
	}
	if err := deps.Err(); err != nil {
		return nil, err
	}
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

// FailOverdueJobs is the server-side timeout backstop: running jobs past
// their (or the default) timeout plus a grace period are failed even if the
// runner keeps heartbeating — covers runners that fail to enforce the kill.
func (s *Store) FailOverdueJobs(ctx context.Context) (int64, error) {
	const grace = 2 * time.Minute
	tag, err := s.pool.Exec(ctx,
		`UPDATE jobs SET status='failed', finished_at=now()
		 WHERE status='running'
		   AND started_at + make_interval(secs =>
		       (CASE WHEN timeout_seconds > 0 THEN timeout_seconds ELSE $1 END) + $2) < now()`,
		int(s.defaultJobTimeout.Seconds()), int(grace.Seconds()))
	return tag.RowsAffected(), err
}

// FailStuckPending fails jobs that no runner picked up within the queue
// timeout (usually a tag routing mistake — no runner matches).
func (s *Store) FailStuckPending(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE jobs j SET status='failed', finished_at=now()
		 WHERE j.status='pending' AND EXISTS (
		   SELECT 1 FROM pipelines p WHERE p.id = j.pipeline_id
		   AND p.created_at + $1::interval < now())`,
		s.queueTimeout.String())
	return tag.RowsAffected(), err
}
