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
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/priytamjeepandey/forge-ci/internal/compiler"
	"github.com/priytamjeepandey/forge-ci/internal/githubapp"
	"github.com/priytamjeepandey/forge-ci/internal/oidc"
	"github.com/priytamjeepandey/forge-ci/internal/proto"
	"github.com/priytamjeepandey/forge-ci/internal/secret"
	"github.com/priytamjeepandey/forge-ci/internal/vcs"
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
	pool       *pgxpool.Pool
	cipher     *secret.Cipher    // envelope encryption for secrets at rest
	appMinter  *githubapp.Minter // mints/caches GitHub App installation tokens
	oidcSigner *oidc.Signer      // mints per-job OIDC ID tokens (keyless cloud auth)
	fetcher    *vcs.Fetcher      // fetches in-repo .forge-ci.yml at the event sha (config-from-repo)

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

// migrationLockKey is a fixed application-defined key for the migration
// advisory lock (ASCII "FoRg"). Any constant works as long as it's stable.
const migrationLockKey int64 = 0x466f5267

// runMigrations applies the embedded schema under a transaction-scoped advisory
// lock so that concurrent server replicas starting against the same database
// serialize: the first replica migrates while the others block on the lock,
// then run the (idempotent) statements as no-ops. Without this, two replicas
// racing CREATE TABLE IF NOT EXISTS on a fresh DB can collide in the Postgres
// catalog (duplicate pg_type). DDL runs in one transaction (Postgres has
// transactional DDL), and pg_advisory_xact_lock auto-releases on commit/rollback
// — no lingering lock on a pooled connection.
func runMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("migrations: begin: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", migrationLockKey); err != nil {
		return fmt.Errorf("migrations: acquire lock: %w", err)
	}
	if _, err := tx.Exec(ctx, migrations); err != nil {
		return fmt.Errorf("migrations: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("migrations: commit: %w", err)
	}
	return nil
}

func New(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("postgres ping: %w", err)
	}
	if err := runMigrations(ctx, pool); err != nil {
		return nil, err
	}
	cipher, err := secret.FromEnv()
	if err != nil {
		return nil, fmt.Errorf("secret cipher: %w", err)
	}
	s := &Store{
		pool:              pool,
		cipher:            cipher,
		appMinter:         githubapp.New(),
		fetcher:           vcs.NewFetcher(),
		defaultJobTimeout: envDuration("DEFAULT_JOB_TIMEOUT", time.Hour),
		maxJobTimeout:     envDuration("MAX_JOB_TIMEOUT", 4*time.Hour),
		queueTimeout:      envDuration("QUEUE_TIMEOUT", 24*time.Hour),
	}
	if err := s.initSecrets(ctx); err != nil {
		return nil, err
	}
	signer, err := s.loadOrCreateOIDCSigner(ctx)
	if err != nil {
		return nil, fmt.Errorf("oidc signing key: %w", err)
	}
	s.oidcSigner = signer
	return s, nil
}

// OIDCSigner returns the process-wide OIDC signer (for the JWKS/discovery
// endpoints). Never nil after New succeeds.
func (s *Store) OIDCSigner() *oidc.Signer { return s.oidcSigner }

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
		  (SELECT count(*) FROM repo_registry  WHERE github_app_private_key <> '' AND github_app_private_key NOT LIKE 'enc:v1:%') +
		  (SELECT count(*) FROM sso_providers  WHERE client_secret <> '' AND client_secret NOT LIKE 'enc:v1:%') +
		  (SELECT count(*) FROM oidc_keys      WHERE private_key_pem_enc <> '' AND private_key_pem_enc NOT LIKE 'enc:v1:%')`).
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
		{"repo_registry", "repo", "github_app_private_key"},
		{"sso_providers", "provider", "client_secret"},
		{"oidc_keys", "kid", "private_key_pem_enc"},
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
// config version it was compiled from (nil = one-off custom config). When
// autoCancel is true, older non-terminal pipelines for the same repo+ref are
// canceled in the same transaction (GitLab-style redundant-pipeline cancel).
// failFast is persisted on the pipeline; when true the scheduler's
// fail_fast_cancel transition stops the pipeline's other non-terminal jobs on
// the first genuine job failure.
func (s *Store) CreatePipeline(ctx context.Context, req proto.CreatePipelineRequest, jobs []compiler.CompiledJob, configVersion *int, autoCancel, failFast bool) (*proto.Pipeline, error) {
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

	// config_source notes where config_yaml came from (audit/retry clarity):
	// 'repo' for an in-repo .forge-ci.yml, else 'registered' (a registry version
	// or a manual one-off). Empty defaults to 'registered' so untouched callers
	// (the manual API path) are unaffected.
	configSource := req.ConfigSource
	if configSource == "" {
		configSource = "registered"
	}
	// source is the trigger (api|push|webhook|merge_request|schedule); untouched
	// callers (manual API path) default to 'api'.
	source := req.Source
	if source == "" {
		source = "api"
	}

	var p proto.Pipeline
	p.Repo, p.Ref, p.SHA = req.Repo, req.Ref, req.SHA
	p.Status = "created"
	p.ConfigVersion = configVersion
	p.Source = source
	err = tx.QueryRow(ctx,
		`INSERT INTO pipelines (repo, ref, sha, config_yaml, triggered_by, config_version, fail_fast, config_source, source, mr_iid, mr_base_sha)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING id, created_at`,
		req.Repo, req.Ref, req.SHA, req.Config, req.TriggeredBy, configVersion, failFast, configSource, source,
		req.MRIID, req.MRBaseSHA).Scan(&p.ID, &p.CreatedAt)
	if err != nil {
		return nil, err
	}

	ids := map[string]int64{}
	for _, j := range jobs {
		env, _ := json.Marshal(j.Env)
		artifacts, _ := json.Marshal(j.ArtifactPaths)
		reportJUnit, _ := json.Marshal(j.ReportJUnit)
		cachePaths, _ := json.Marshal(j.CachePaths)
		cacheKeyFiles, _ := json.Marshal(j.CacheKeyFiles)
		services, _ := json.Marshal(j.Services)
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
		// Manual jobs start gated in the 'blocked' state (with the manual flag so
		// the scheduler and approval endpoint distinguish them from an
		// environment-approval block); a play releases them back to 'created'.
		status := "created"
		var blockedAt any
		if j.Manual {
			status = "blocked"
			blockedAt = time.Now()
		}
		var id int64
		err = tx.QueryRow(ctx,
			`INSERT INTO jobs (pipeline_id, name, stage, stage_idx, image, script, env, environment, tags, artifact_paths, artifact_expire_seconds, report_junit, cache_paths, cache_key, cache_key_files, cache_policy, services, network, timeout_seconds, max_attempts, status, manual, allow_failure, blocked_at)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24) RETURNING id`,
			p.ID, j.Name, j.Stage, j.StageIdx, image, j.Script, env, environment, tags, artifacts, j.ArtifactExpireSeconds, reportJUnit, cachePaths, j.CacheKey, cacheKeyFiles, j.CachePolicy, services, j.Network, timeoutSec, j.Retry+1, status, j.Manual, j.AllowFailure, blockedAt).Scan(&id)
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

	// Auto-cancel superseded pipelines: older, non-terminal pipelines for the
	// exact same repo+ref (never across refs). Non-running jobs go straight to
	// canceled; running jobs are flagged cancel_requested so their runner stops
	// them on the next heartbeat.
	if autoCancel {
		if _, err := tx.Exec(ctx,
			`UPDATE jobs j SET status='canceled', finished_at=now()
			 FROM pipelines op
			 WHERE j.pipeline_id = op.id AND op.repo=$1 AND op.ref=$2 AND op.id < $3
			   AND j.status IN ('created','pending','blocked')`,
			p.Repo, p.Ref, p.ID); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE jobs j SET cancel_requested=TRUE
			 FROM pipelines op
			 WHERE j.pipeline_id = op.id AND op.repo=$1 AND op.ref=$2 AND op.id < $3
			   AND j.status='running'`,
			p.Repo, p.Ref, p.ID); err != nil {
			return nil, err
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
		`SELECT p.id, p.repo, p.ref, p.sha, p.config_version, p.source, p.created_at,
		        COALESCE(json_agg(json_build_object(
		            'stage', j.stage, 'idx', j.stage_idx, 'status', CASE WHEN j.status='failed' AND j.allow_failure THEN 'success' ELSE j.status END
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
		if err := rows.Scan(&p.ID, &p.Repo, &p.Ref, &p.SHA, &p.ConfigVersion, &p.Source, &p.CreatedAt, &raw); err != nil {
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

// ListPipelinesPage returns one page of pipelines (optionally filtered by repo),
// newest first, plus the total count for the filter so the UI can paginate.
// limit/offset are assumed already clamped by the caller.
func (s *Store) ListPipelinesPage(ctx context.Context, repo string, limit, offset int) ([]proto.Pipeline, int, error) {
	var total int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM pipelines WHERE ($1 = '' OR repo = $1)`, repo).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT p.id, p.repo, p.ref, p.sha, p.config_version, p.source, p.created_at,
		        COALESCE(json_agg(json_build_object(
		            'stage', j.stage, 'idx', j.stage_idx, 'status', CASE WHEN j.status='failed' AND j.allow_failure THEN 'success' ELSE j.status END
		        ) ORDER BY j.stage_idx) FILTER (WHERE j.id IS NOT NULL), '[]')
		 FROM pipelines p LEFT JOIN jobs j ON j.pipeline_id = p.id
		 WHERE ($1 = '' OR p.repo = $1)
		 GROUP BY p.id ORDER BY p.id DESC LIMIT $2 OFFSET $3`, repo, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := []proto.Pipeline{}
	for rows.Next() {
		var p proto.Pipeline
		var raw []byte
		if err := rows.Scan(&p.ID, &p.Repo, &p.Ref, &p.SHA, &p.ConfigVersion, &p.Source, &p.CreatedAt, &raw); err != nil {
			return nil, 0, err
		}
		var sjs []stageJob
		if err := json.Unmarshal(raw, &sjs); err != nil {
			return nil, 0, err
		}
		statuses := make([]string, len(sjs))
		for i, sj := range sjs {
			statuses[i] = sj.Status
		}
		p.Status = deriveStatus(statuses)
		p.Stages = deriveStages(sjs)
		out = append(out, p)
	}
	return out, total, rows.Err()
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
		`SELECT id, repo, ref, sha, config_version, source, created_at FROM pipelines WHERE id=$1`, id).
		Scan(&p.ID, &p.Repo, &p.Ref, &p.SHA, &p.ConfigVersion, &p.Source, &p.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}

	rows, err := s.pool.Query(ctx,
		`SELECT j.id, j.pipeline_id, j.name, j.stage, j.stage_idx, j.image, j.environment,
		        j.status,
		        CASE WHEN j.status='failed' AND j.allow_failure THEN 'success' ELSE j.status END,
		        j.started_at, j.finished_at, j.exit_code, j.manual, j.allow_failure,
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
		// effective status folds an allowed failure into 'success' for pipeline
		// and stage status derivation, while j.Status keeps the true state.
		var effective string
		if err := rows.Scan(&j.ID, &j.PipelineID, &j.Name, &j.Stage, &j.StageIdx, &j.Image,
			&j.Environment, &j.Status, &effective, &j.StartedAt, &j.FinishedAt, &j.ExitCode,
			&j.Manual, &j.AllowFailure, &j.Needs); err != nil {
			return nil, nil, err
		}
		jobs = append(jobs, j)
		statuses = append(statuses, effective)
		sjs = append(sjs, stageJob{Stage: j.Stage, Idx: j.StageIdx, Status: effective})
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
		        j.status, j.started_at, j.finished_at, j.exit_code, j.manual, j.allow_failure,
		        COALESCE(array_agg(n.needs_job_id) FILTER (WHERE n.needs_job_id IS NOT NULL), '{}')
		 FROM jobs j LEFT JOIN job_needs n ON n.job_id = j.id
		 WHERE j.id = $1 GROUP BY j.id`, id).
		Scan(&j.ID, &j.PipelineID, &j.Name, &j.Stage, &j.StageIdx, &j.Image,
			&j.Environment, &j.Status, &j.StartedAt, &j.FinishedAt, &j.ExitCode,
			&j.Manual, &j.AllowFailure, &j.Needs)
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

// TruncationNotice is the single line written (once) when a job's cumulative
// log bytes cross MAX_JOB_LOG_BYTES. Both log backends emit exactly this text so
// truncated output is byte-identical regardless of where logs are buffered.
func TruncationNotice(capBytes int64) string {
	return fmt.Sprintf(
		"\n[forge] log truncated: job exceeded MAX_JOB_LOG_BYTES (%d bytes); further output dropped\n",
		capBytes)
}

// AppendLogCapped appends a log chunk while enforcing a cumulative per-job
// byte cap. capBytes <= 0 disables the cap. Once the cap is crossed it writes a
// single truncation notice, sets jobs.log_truncated, and silently drops all
// further chunks. Returns the cumulative byte total after the append and
// whether truncation fired on THIS call. Returns ErrNotFound if the job does
// not exist. This is the postgres log backend's write path (see
// internal/logstore).
func (s *Store) AppendLogCapped(ctx context.Context, jobID int64, chunk string, capBytes int64) (total int64, truncatedNow bool, err error) {
	if capBytes <= 0 {
		if err := s.AppendLog(ctx, jobID, chunk); err != nil {
			return 0, false, err
		}
		return 0, false, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback(ctx)

	var truncated bool
	var used int64
	err = tx.QueryRow(ctx,
		`SELECT j.log_truncated,
		        COALESCE((SELECT sum(octet_length(chunk)) FROM job_logs WHERE job_id=j.id), 0)
		 FROM jobs j WHERE j.id=$1`, jobID).Scan(&truncated, &used)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, ErrNotFound
	}
	if err != nil {
		return 0, false, err
	}
	if truncated {
		return used, false, tx.Commit(ctx) // already capped — drop
	}
	if used+int64(len(chunk)) > capBytes {
		// Store the portion that still fits under the cap, then a single
		// truncation notice, and mark the job so further chunks are dropped.
		if remaining := capBytes - used; remaining > 0 {
			if _, err := tx.Exec(ctx,
				`INSERT INTO job_logs (job_id, chunk) VALUES ($1,$2)`,
				jobID, chunk[:remaining]); err != nil {
				return 0, false, err
			}
			used += remaining
		}
		notice := TruncationNotice(capBytes)
		if _, err := tx.Exec(ctx,
			`INSERT INTO job_logs (job_id, chunk) VALUES ($1,$2)`, jobID, notice); err != nil {
			return 0, false, err
		}
		used += int64(len(notice))
		if _, err := tx.Exec(ctx,
			`UPDATE jobs SET log_truncated=TRUE WHERE id=$1`, jobID); err != nil {
			return 0, false, err
		}
		return used, true, tx.Commit(ctx)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO job_logs (job_id, chunk) VALUES ($1,$2)`, jobID, chunk); err != nil {
		return 0, false, err
	}
	return used + int64(len(chunk)), false, tx.Commit(ctx)
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

// DeleteExpiredWebhookDeliveries bounds the dedup table by dropping delivery
// records older than the retention window (providers never redeliver that far
// back).
func (s *Store) DeleteExpiredWebhookDeliveries(ctx context.Context, olderThan time.Duration) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM webhook_deliveries WHERE received_at < now() - $1::interval`, olderThan.String())
	return tag.RowsAffected(), err
}

// ---- webhook delivery dedup ----

// RecordWebhookDelivery records a (provider, delivery_id) and reports whether it
// was newly inserted. A false return means this delivery was already seen — the
// caller must NOT create a second pipeline for it.
func (s *Store) RecordWebhookDelivery(ctx context.Context, provider, deliveryID string) (isNew bool, err error) {
	tag, err := s.pool.Exec(ctx,
		`INSERT INTO webhook_deliveries (provider, delivery_id) VALUES ($1,$2)
		 ON CONFLICT (provider, delivery_id) DO NOTHING`, provider, deliveryID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ForgetWebhookDelivery removes a delivery record, letting a redelivery be
// retried. Used to roll back the dedup gate when pipeline creation fails after
// the delivery was recorded.
func (s *Store) ForgetWebhookDelivery(ctx context.Context, provider, deliveryID string) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM webhook_deliveries WHERE provider=$1 AND delivery_id=$2`, provider, deliveryID)
	return err
}

func (s *Store) GetLogs(ctx context.Context, jobID int64) (string, error) {
	var logs string
	err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(string_agg(chunk, '' ORDER BY id), '') FROM job_logs WHERE job_id=$1`,
		jobID).Scan(&logs)
	if err != nil {
		return "", err
	}
	// Backstop masking: mask again over the fully-reassembled log. This catches
	// a secret that was split across chunk boundaries (each chunk individually
	// unmatched) or one stored unmasked for any other reason. Cheap: masked
	// values are few and short.
	masked, merr := s.MaskedValuesForJob(ctx, jobID)
	if merr == nil {
		for _, v := range masked {
			if v != "" {
				logs = strings.ReplaceAll(logs, v, "[MASKED]")
			}
		}
	}
	return logs, nil
}

// ---- log archive pointer (LOG_BACKEND=redis) ----

// JobLogMeta bundles the fields the log read/archive paths need without pulling
// the whole job row: current status, the archive pointer (nil objectKey = not
// archived), the archived byte length, and the truncation flag.
type JobLogMeta struct {
	Status    string
	ObjectKey *string
	TotalByte int64
	Truncated bool
}

// GetJobLogMeta returns the log-relevant fields for a job. ErrNotFound when the
// job does not exist.
func (s *Store) GetJobLogMeta(ctx context.Context, jobID int64) (JobLogMeta, error) {
	var m JobLogMeta
	err := s.pool.QueryRow(ctx,
		`SELECT status, log_object_key, log_total_bytes, log_truncated
		 FROM jobs WHERE id=$1`, jobID).
		Scan(&m.Status, &m.ObjectKey, &m.TotalByte, &m.Truncated)
	if errors.Is(err, pgx.ErrNoRows) {
		return m, ErrNotFound
	}
	return m, err
}

// SetLogPointer records where a finished job's archived log landed in the blob
// store (objectKey may be "" when the job produced no output), its byte length,
// and the truncation flag. Setting a non-NULL object key marks the job archived
// so the scheduler's safety-net sweep skips it.
func (s *Store) SetLogPointer(ctx context.Context, jobID int64, objectKey string, totalBytes int64, truncated bool) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE jobs SET log_object_key=$2, log_total_bytes=$3, log_truncated=$4 WHERE id=$1`,
		jobID, objectKey, totalBytes, truncated)
	return err
}

// TerminalUnarchivedJobs returns ids of jobs that reached a terminal state but
// were never archived (log_object_key IS NULL) within the recent window. It is
// the scheduler's safety net for jobs that die WITHOUT a runner complete call
// (stale/overdue/canceled-while-pending), whose Redis buffers would otherwise
// expire unflushed. Bounded by `within` so we never rescan ancient history.
func (s *Store) TerminalUnarchivedJobs(ctx context.Context, within time.Duration, limit int) ([]int64, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id FROM jobs
		 WHERE status IN ('success','failed','canceled')
		   AND log_object_key IS NULL
		   AND finished_at IS NOT NULL
		   AND finished_at > now() - $1::interval
		 ORDER BY finished_at LIMIT $2`, within.String(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ExpiredLogObjectKeys returns the blob keys of archived job logs belonging to
// pipelines older than the retention window, so the caller can delete the
// objects from the blob store before the DB rows cascade away. Empty-string
// keys (jobs that produced no output) are skipped.
func (s *Store) ExpiredLogObjectKeys(ctx context.Context, olderThan time.Duration) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT j.log_object_key FROM jobs j
		 JOIN pipelines p ON p.id = j.pipeline_id
		 WHERE p.created_at < now() - $1::interval
		   AND j.log_object_key IS NOT NULL AND j.log_object_key <> ''`, olderThan.String())
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
	var envRaw, artifactsRaw, reportJUnitRaw, cachePathsRaw, cacheKeyFilesRaw, servicesRaw []byte
	var cachePolicy string
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
		           j.artifact_paths, j.report_junit, j.cache_paths, j.cache_key, j.cache_key_files,
		           j.cache_policy, j.services, j.network, j.environment, j.timeout_seconds,
		           p.repo, p.ref, p.sha`,
		req.RunnerID, runnerTags).
		Scan(&j.ID, &j.PipelineID, &j.Name, &image, &j.Script, &envRaw,
			&artifactsRaw, &reportJUnitRaw, &cachePathsRaw, &j.CacheKey, &cacheKeyFilesRaw,
			&cachePolicy, &servicesRaw, &j.Network, &environment, &j.TimeoutSeconds, &repo, &ref, &sha)
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

	// Keyless cloud auth: mint a short-lived, per-job OIDC ID token and inject it
	// as FORGE_OIDC_TOKEN (with the GitLab-style CI_JOB_JWT alias). The job
	// exchanges it for AWS/GCP credentials — no static cloud keys. It is a
	// secret-equivalent, so it is added to RedactValues to mask it from logs.
	// Minting never fails job acquisition (empty token when unavailable).
	if tok := s.mintJobOIDCToken(repo, ref, sha, env, j.PipelineID, j.ID); tok != "" {
		j.Env["FORGE_OIDC_TOKEN"] = tok
		j.Env["CI_JOB_JWT"] = tok
		j.RedactValues = append(j.RedactValues, tok)
	}
	j.ArtifactPaths = []string{}
	_ = json.Unmarshal(artifactsRaw, &j.ArtifactPaths)
	j.ReportJUnitPaths = []string{}
	_ = json.Unmarshal(reportJUnitRaw, &j.ReportJUnitPaths)
	j.CachePaths = []string{}
	_ = json.Unmarshal(cachePathsRaw, &j.CachePaths)
	j.CacheKeyFiles = []string{}
	_ = json.Unmarshal(cacheKeyFilesRaw, &j.CacheKeyFiles)
	j.CachePolicy = cachePolicy
	_ = json.Unmarshal(servicesRaw, &j.Services)

	// Source checkout info when the repo is registered against a real VCS.
	cloneURL, token, conn, err := s.cloneTarget(ctx, repo)
	if err != nil {
		return nil, err
	}
	if cloneURL != "" {
		j.CloneURL, j.SHA, j.Ref, j.RepoName = cloneURL, sha, ref, repo
		if token != "" {
			j.RedactValues = append(j.RedactValues, token)
		}
		// CodeCommit hands the runner an unauthenticated URL and the AWS context
		// to sign it with. The credential is minted on the runner from the OIDC
		// token above, so it never travels over the runner protocol at all.
		if conn.Provider == "codecommit" {
			if cc, ok := conn.codeCommitRepo(); ok {
				j.CloneAuth = proto.CloneAuthAWSSigV4
				j.AWSRegion = cc.Region
				j.AWSRoleARN = conn.AWS.RoleARN
			}
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

// Heartbeat stamps the job's liveness and returns whether a cancellation has
// been requested for it. The runner uses the returned flag to stop a running
// job (killing the process group / container / pod) and report 'canceled'.
func (s *Store) Heartbeat(ctx context.Context, jobID int64) (cancel bool, err error) {
	err = s.pool.QueryRow(ctx,
		`UPDATE jobs SET heartbeat_at=now()
		 WHERE id=$1 AND status='running' RETURNING cancel_requested`, jobID).Scan(&cancel)
	if errors.Is(err, pgx.ErrNoRows) {
		// Job is no longer running (already finished/canceled). Not an error
		// for the runner; it will learn the outcome at complete time.
		return false, nil
	}
	return cancel, err
}

// CompleteJob transitions a running job to its terminal (or requeued) state and
// returns the resulting status. It centralizes three cross-cutting behaviors:
//
//   - cancellation: a job whose cancel was requested ends 'canceled' (whether
//     the runner reported 'canceled' or 'failed' after the kill); never retried.
//   - graceful drain: status 'requeue' returns the job to 'pending' for another
//     runner, preserving the attempt count (not a retry).
//   - retries: a plain failure (non-timeout: exit != 124) with attempts left is
//     requeued as the next attempt instead of failing the pipeline.
//
// It is idempotent: a job that already left 'running' yields ErrNotFound so a
// late/duplicate report from the runner is a no-op.
//
// On a retry it returns a non-empty note (the "attempt N/M …" separator) which
// the caller appends to the job's log through the active log backend, so the
// separator lands in the right place whether logs live in Postgres or Redis
// (CompleteJob no longer writes log bodies itself — see internal/logstore).
func (s *Store) CompleteJob(ctx context.Context, jobID int64, status string, exitCode int) (final, note string, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback(ctx)

	var attempt, maxAttempts int
	var cancelReq bool
	var environment *string
	var pipelineID int64
	var repo, ref, sha, triggeredBy string
	err = tx.QueryRow(ctx,
		`SELECT j.attempt, j.max_attempts, j.cancel_requested, j.environment,
		        p.id, p.repo, p.ref, p.sha, p.triggered_by
		 FROM jobs j JOIN pipelines p ON p.id = j.pipeline_id
		 WHERE j.id=$1 AND j.status='running' FOR UPDATE OF j`, jobID).
		Scan(&attempt, &maxAttempts, &cancelReq, &environment, &pipelineID, &repo, &ref, &sha, &triggeredBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrNotFound
	}
	if err != nil {
		return "", "", err
	}

	// requeue for graceful drain: back to the queue, same attempt.
	if status == "requeue" {
		if _, err := tx.Exec(ctx,
			`UPDATE jobs SET status='pending', started_at=NULL, heartbeat_at=NULL,
			        runner_id=NULL, exit_code=NULL WHERE id=$1`, jobID); err != nil {
			return "", "", err
		}
		return "pending", "", tx.Commit(ctx)
	}

	// Cancellation wins over the reported status: a killed job may surface as
	// 'failed' or 'canceled' — either way it ends canceled and is not retried.
	if status == "canceled" || cancelReq {
		if _, err := tx.Exec(ctx,
			`UPDATE jobs SET status='canceled', exit_code=$2, finished_at=now(),
			        cancel_requested=FALSE WHERE id=$1`, jobID, exitCode); err != nil {
			return "", "", err
		}
		return "canceled", "", tx.Commit(ctx)
	}

	// Retry a plain failure (not a timeout-kill: exit 124) while attempts remain.
	if status == "failed" && exitCode != 124 && attempt < maxAttempts {
		if _, err := tx.Exec(ctx,
			`UPDATE jobs SET status='pending', attempt=attempt+1, started_at=NULL,
			        heartbeat_at=NULL, runner_id=NULL, exit_code=NULL WHERE id=$1`, jobID); err != nil {
			return "", "", err
		}
		note = fmt.Sprintf("\n[forge] attempt %d/%d failed (exit %d); retrying (attempt %d/%d)\n",
			attempt, maxAttempts, exitCode, attempt+1, maxAttempts)
		return "pending", note, tx.Commit(ctx)
	}

	if _, err := tx.Exec(ctx,
		`UPDATE jobs SET status=$2, exit_code=$3, finished_at=now()
		 WHERE id=$1`, jobID, status, exitCode); err != nil {
		return "", "", err
	}

	// Deployment record: when an environment-targeting job flips to success, that
	// IS a deployment. Insert it in the SAME transaction as the status flip so the
	// two commit together (crash-safe). UNIQUE(job_id) + ON CONFLICT DO NOTHING
	// dedupes: a given job's success is recorded at most once, even on a late or
	// duplicated runner report. deployed_by is the environment approver when the
	// job was approval-gated, otherwise the pipeline's triggered_by.
	if status == "success" && environment != nil && *environment != "" {
		if _, err := tx.Exec(ctx,
			`INSERT INTO deployments
			     (repo, environment, sha, ref, pipeline_id, job_id, deployed_by, status)
			 VALUES ($1,$2,$3,$4,$5,$6,
			         COALESCE((SELECT approver FROM job_approvals
			                   WHERE job_id=$6 AND verdict='approved'
			                   ORDER BY id DESC LIMIT 1), $7),
			         'success')
			 ON CONFLICT (job_id) DO NOTHING`,
			repo, *environment, sha, ref, pipelineID, jobID, triggeredBy); err != nil {
			return "", "", err
		}
	}
	return status, "", tx.Commit(ctx)
}

// ---- cancellation ----

var terminalStatuses = []string{"success", "failed", "canceled"}

func isTerminal(status string) bool {
	for _, t := range terminalStatuses {
		if status == t {
			return true
		}
	}
	return false
}

// CancelJob cancels a single job by its current state and returns the job.
// created/pending/blocked -> canceled immediately; running -> cancel_requested
// (the runner stops it and reports 'canceled'); terminal states are a no-op
// (idempotent). Dependents of a canceled job are cascaded by the scheduler
// (CancelDeadJobs). Returns ErrNotFound if the job does not exist.
func (s *Store) CancelJob(ctx context.Context, jobID int64) (*proto.Job, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var status string
	err = tx.QueryRow(ctx,
		`SELECT status FROM jobs WHERE id=$1 FOR UPDATE`, jobID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	switch {
	case isTerminal(status):
		// no-op, return current state
	case status == "running":
		if _, err := tx.Exec(ctx,
			`UPDATE jobs SET cancel_requested=TRUE WHERE id=$1`, jobID); err != nil {
			return nil, err
		}
	default: // created, pending, blocked
		if _, err := tx.Exec(ctx,
			`UPDATE jobs SET status='canceled', finished_at=now() WHERE id=$1`, jobID); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.GetJob(ctx, jobID)
}

// ErrNotManual is returned when a play is attempted on a job that is not a
// gated manual job awaiting its trigger.
var ErrNotManual = errors.New("job is not a manual job awaiting play")

// StartManualJob releases a gated manual job. A manual job is created in the
// 'blocked' state with manual=TRUE; play returns it to 'created' and clears the
// manual flag so the normal scheduler flow (needs, then protected-environment
// approval if any) applies from here on. Idempotency: a job already past the
// gate returns ErrNotManual.
func (s *Store) StartManualJob(ctx context.Context, jobID int64) (*proto.Job, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var status string
	var manual bool
	err = tx.QueryRow(ctx,
		`SELECT status, manual FROM jobs WHERE id=$1 FOR UPDATE`, jobID).Scan(&status, &manual)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if status != "blocked" || !manual {
		return nil, ErrNotManual
	}
	if _, err := tx.Exec(ctx,
		`UPDATE jobs SET status='created', manual=FALSE, blocked_at=NULL WHERE id=$1`, jobID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.GetJob(ctx, jobID)
}

// CancelPipeline cancels every non-terminal job in a pipeline (and thereby its
// dependents). Running jobs are flagged for the runner to stop; others go
// straight to canceled. Idempotent: an already-finished pipeline is a clean
// no-op. Returns ErrNotFound if the pipeline does not exist.
func (s *Store) CancelPipeline(ctx context.Context, pipelineID int64) (*proto.Pipeline, []proto.Job, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback(ctx)

	var exists bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pipelines WHERE id=$1)`, pipelineID).Scan(&exists)
	if err != nil {
		return nil, nil, err
	}
	if !exists {
		return nil, nil, ErrNotFound
	}
	if _, err := tx.Exec(ctx,
		`UPDATE jobs SET status='canceled', finished_at=now()
		 WHERE pipeline_id=$1 AND status IN ('created','pending','blocked')`, pipelineID); err != nil {
		return nil, nil, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE jobs SET cancel_requested=TRUE
		 WHERE pipeline_id=$1 AND status='running'`, pipelineID); err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, nil, err
	}
	return s.GetPipeline(ctx, pipelineID)
}

// ---- scheduler transitions (each idempotent; called every tick) ----

// A need is satisfied when the dependency succeeded, or failed while marked
// allow_failure (GitLab: an allowed failure does not block dependents).
const needsUnmet = `EXISTS (
	SELECT 1 FROM job_needs n JOIN jobs d ON d.id = n.needs_job_id
	WHERE n.job_id = j.id
	  AND d.status <> 'success'
	  AND NOT (d.status = 'failed' AND d.allow_failure))`

// depFatallyDead is true when a dependency reached a terminal non-success state
// that should NOT be tolerated: canceled, or failed without allow_failure.
const depFatallyDead = `EXISTS (
	SELECT 1 FROM job_needs n JOIN jobs d ON d.id = n.needs_job_id
	WHERE n.job_id = j.id
	  AND (d.status = 'canceled' OR (d.status = 'failed' AND NOT d.allow_failure)))`

// CancelDeadJobs cancels jobs whose dependencies fatally failed or were
// canceled. Covers both queued (created) jobs and gated manual (blocked) jobs
// so a manual job whose upstream died is not left hanging forever. An allowed
// failure upstream does not trigger cancellation.
func (s *Store) CancelDeadJobs(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE jobs j SET status='canceled', finished_at=now()
		 WHERE (j.status='created' OR (j.status='blocked' AND j.manual))
		   AND `+depFatallyDead)
	return tag.RowsAffected(), err
}

// PromoteReadyJobs moves created jobs with all needs satisfied to pending, or
// to blocked when they target a protected environment.
// protectedFor matches a job's environment against a repo-specific rule or
// the global (repo=”) default.
const protectedFor = `EXISTS (
	SELECT 1 FROM protected_environments pe, pipelines p
	WHERE p.id = j.pipeline_id AND pe.name = j.environment AND pe.repo IN ('', p.repo))`

// frozenEnv is true when the job targets an environment currently inside a
// deploy-freeze window (repo-specific or global). A frozen job is left in
// 'created' (not promoted) until the window passes, at which point a later tick
// promotes it normally — self-releasing, crash-safe, no extra state.
const frozenEnv = `EXISTS (
	SELECT 1 FROM deploy_freezes df, pipelines p
	WHERE p.id = j.pipeline_id AND df.environment = j.environment
	  AND df.repo IN ('', p.repo)
	  AND now() >= df.starts_at AND now() < df.ends_at)`

func (s *Store) PromoteReadyJobs(ctx context.Context) (int64, error) {
	blocked, err := s.pool.Exec(ctx,
		`UPDATE jobs j SET status='blocked', blocked_at=now()
		 WHERE j.status='created' AND `+protectedFor+` AND NOT `+needsUnmet+` AND NOT `+frozenEnv)
	if err != nil {
		return 0, err
	}
	pending, err := s.pool.Exec(ctx,
		`UPDATE jobs j SET status='pending'
		 WHERE j.status='created' AND NOT `+protectedFor+` AND NOT `+needsUnmet+` AND NOT `+frozenEnv)
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
		 WHERE j.status='blocked' AND NOT j.manual
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

// hasGenuineFailure is true when a pipeline has at least one job in a genuine
// terminal failure: status='failed' AND NOT allow_failure. An allowed failure
// must NOT trigger fail-fast, and a retrying attempt is represented as 'pending'
// (CompleteJob requeues it without ever writing status='failed'), so only a
// FINAL, retries-exhausted failure of a non-allow_failure job matches here.
const hasGenuineFailure = `EXISTS (
	SELECT 1 FROM jobs f
	WHERE f.pipeline_id = j.pipeline_id
	  AND f.status='failed' AND NOT f.allow_failure)`

// FailFastCancel implements opt-in pipeline-level fail-fast. For every pipeline
// flagged fail_fast whose jobs include a genuine failure (see hasGenuineFailure),
// it stops that pipeline's OTHER non-terminal jobs — not just downstream
// dependents:
//
//   - created/pending/blocked siblings go straight to 'canceled'.
//   - running siblings (including allow_failure ones — the pipeline is doomed)
//     are flagged cancel_requested so their runner kills them and reports
//     'canceled' on the next heartbeat, reusing the same mechanism as CancelJob.
//
// The already-failed job and any terminal job are naturally excluded (their
// status is not in the created/pending/blocked/running sets). The running
// update skips jobs already flagged, so the statement is idempotent and safe to
// re-run every tick and across replicas. Returns the number of sibling jobs
// canceled or newly flagged this call.
func (s *Store) FailFastCancel(ctx context.Context) (int64, error) {
	nonRunning, err := s.pool.Exec(ctx,
		`UPDATE jobs j SET status='canceled', finished_at=now()
		 FROM pipelines p
		 WHERE j.pipeline_id = p.id AND p.fail_fast
		   AND j.status IN ('created','pending','blocked')
		   AND `+hasGenuineFailure)
	if err != nil {
		return 0, err
	}
	running, err := s.pool.Exec(ctx,
		`UPDATE jobs j SET cancel_requested=TRUE
		 FROM pipelines p
		 WHERE j.pipeline_id = p.id AND p.fail_fast
		   AND j.status='running' AND NOT j.cancel_requested
		   AND `+hasGenuineFailure)
	if err != nil {
		return 0, err
	}
	return nonRunning.RowsAffected() + running.RowsAffected(), nil
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
