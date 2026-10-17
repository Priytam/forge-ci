package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

var (
	// ErrNotRetryable: only a failed or canceled (live) job can be retried.
	ErrNotRetryable = errors.New("only a failed or canceled job can be retried")
	// ErrRetryRefused: the environment policy would refuse the job on its ref.
	ErrRetryRefused = errors.New("environment policy refuses this job")
)

// jobSpecCols are the columns copied from a job to its retry: everything that
// defines what to run, nothing about how the last attempt went.
const jobSpecCols = `pipeline_id, name, stage, stage_idx, image, script, env, environment, tags,
	artifact_paths, artifact_expire_seconds, report_junit, cache_paths, cache_key, cache_key_files,
	cache_policy, services, network, timeout_seconds, max_attempts, allow_failure, resource_group, affinity_key,
	environment_url, report_dotenv, coverage_regex, coverage_report, secrets, registry_logins`

// RetryResult says which jobs a retry re-queued: old id → new id.
type RetryResult struct {
	PipelineID int64           `json:"pipeline_id"`
	Repo       string          `json:"repo"`
	Retried    map[int64]int64 `json:"retried"`
}

// RetryJob re-runs one failed or canceled job inside its pipeline, together
// with every downstream job (transitively, via needs) that did not succeed.
// Each re-run job is a NEW row (fresh log, fresh attempt count) with retry_of
// pointing at the old one; the old rows are kept as history and marked
// superseded. Needs between re-run jobs are rewired to the new rows; needs on
// untouched jobs (e.g. a succeeded build) stay as they were. The scheduler
// then runs them normally — approvals, freezes and resource groups apply
// again — and the pipeline's status follows the new jobs.
func (s *Store) RetryJob(ctx context.Context, jobID int64) (*RetryResult, error) {
	return s.RetryJobWith(ctx, jobID, RetryOpts{})
}

// RetryOpts tunes a retry.
type RetryOpts struct {
	// Debug reruns the job (only the one asked for, not its dependents) with
	// CI_DEBUG_TRACE=true, so the runner echoes every command it executes.
	Debug bool
}

func (s *Store) RetryJobWith(ctx context.Context, jobID int64, opts RetryOpts) (*RetryResult, error) {
	policy, err := s.GetEnvPolicy(ctx)
	if err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var res RetryResult
	var status, ref string
	var superseded bool
	err = tx.QueryRow(ctx,
		`SELECT j.pipeline_id, j.status, j.superseded, p.repo, p.ref
		 FROM jobs j JOIN pipelines p ON p.id = j.pipeline_id WHERE j.id = $1`, jobID).
		Scan(&res.PipelineID, &status, &superseded, &res.Repo, &ref)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if superseded || (status != "failed" && status != "canceled") {
		return nil, ErrNotRetryable
	}
	// Lock the pipeline's live jobs so a concurrent retry or transition can't
	// interleave.
	rows, err := tx.Query(ctx,
		`SELECT id, status, COALESCE(environment, '') FROM jobs
		 WHERE pipeline_id = $1 AND NOT superseded ORDER BY id FOR UPDATE`, res.PipelineID)
	if err != nil {
		return nil, err
	}
	jobStatus := map[int64]string{}
	jobEnv := map[int64]string{}
	for rows.Next() {
		var id int64
		var st, env string
		if err := rows.Scan(&id, &st, &env); err != nil {
			rows.Close()
			return nil, err
		}
		jobStatus[id], jobEnv[id] = st, env
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	needs := map[int64][]int64{}      // job → its needs
	dependents := map[int64][]int64{} // job → jobs that need it
	nrows, err := tx.Query(ctx,
		`SELECT n.job_id, n.needs_job_id FROM job_needs n
		 JOIN jobs j ON j.id = n.job_id WHERE j.pipeline_id = $1 AND NOT j.superseded`, res.PipelineID)
	if err != nil {
		return nil, err
	}
	for nrows.Next() {
		var a, b int64
		if err := nrows.Scan(&a, &b); err != nil {
			nrows.Close()
			return nil, err
		}
		needs[a] = append(needs[a], b)
		dependents[b] = append(dependents[b], a)
	}
	nrows.Close()
	if err := nrows.Err(); err != nil {
		return nil, err
	}

	// The job plus its downstream jobs that did not succeed. A dependent that
	// is running or queued (possible after an allowed failure) is left alone.
	set := map[int64]bool{jobID: true}
	queue := []int64{jobID}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, d := range dependents[cur] {
			st := jobStatus[d]
			if set[d] || st == "success" || st == "running" || st == "pending" {
				continue
			}
			set[d] = true
			queue = append(queue, d)
		}
	}
	ids := make([]int64, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, k int) bool { return ids[i] < ids[k] })

	for _, id := range ids {
		if _, denied := policy.CheckJob(jobEnv[id], ref); denied != "" {
			return nil, fmt.Errorf("%w: %s", ErrRetryRefused, denied)
		}
	}

	// Old rows: superseded history; anything not finished is closed out so
	// the scheduler never picks it up.
	if _, err := tx.Exec(ctx,
		`UPDATE jobs SET superseded = TRUE,
		        status = CASE WHEN status IN ('created','blocked') THEN 'canceled' ELSE status END,
		        finished_at = COALESCE(finished_at, now())
		 WHERE id = ANY($1)`, ids); err != nil {
		return nil, err
	}
	res.Retried = map[int64]int64{}
	for _, id := range ids {
		var nid int64
		if err := tx.QueryRow(ctx,
			`INSERT INTO jobs (`+jobSpecCols+`, status, retry_of)
			 SELECT `+jobSpecCols+`, 'created', id FROM jobs WHERE id = $1
			 RETURNING id`, id).Scan(&nid); err != nil {
			return nil, err
		}
		res.Retried[id] = nid
		if opts.Debug && id == jobID {
			if _, err := tx.Exec(ctx,
				`UPDATE jobs SET env = env || '{"CI_DEBUG_TRACE":"true"}'::jsonb WHERE id = $1`, nid); err != nil {
				return nil, err
			}
		}
	}
	for _, id := range ids {
		for _, need := range needs[id] {
			target := need
			if nn, ok := res.Retried[need]; ok {
				target = nn
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO job_needs (job_id, needs_job_id) VALUES ($1, $2)`,
				res.Retried[id], target); err != nil {
				return nil, err
			}
		}
	}
	return &res, tx.Commit(ctx)
}

// expandResourceGroup resolves $CI_ENVIRONMENT_NAME / ${CI_ENVIRONMENT_NAME}
// in a resource_group to the job's (canonical) environment, so one line —
// `resource_group: deploy-$CI_ENVIRONMENT_NAME` — gives one deploy at a time
// per environment.
func expandResourceGroup(group, env string) string {
	if group == "" {
		return ""
	}
	r := strings.NewReplacer("${CI_ENVIRONMENT_NAME}", env, "$CI_ENVIRONMENT_NAME", env)
	return r.Replace(group)
}

// SupersededJobs returns a pipeline's earlier attempts (jobs replaced by a
// "Retry job"), newest first, so their logs stay reachable.
func (s *Store) SupersededJobs(ctx context.Context, pipelineID int64) ([]proto.Job, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, pipeline_id, name, stage, stage_idx, environment, status, started_at,
		        finished_at, exit_code, manual, allow_failure, status_reason, retry_of, resource_group
		 FROM jobs WHERE pipeline_id = $1 AND superseded ORDER BY id DESC`, pipelineID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []proto.Job{}
	for rows.Next() {
		var j proto.Job
		if err := rows.Scan(&j.ID, &j.PipelineID, &j.Name, &j.Stage, &j.StageIdx, &j.Environment,
			&j.Status, &j.StartedAt, &j.FinishedAt, &j.ExitCode, &j.Manual, &j.AllowFailure,
			&j.StatusReason, &j.RetryOf, &j.ResourceGroup); err != nil {
			return nil, err
		}
		j.Needs = []int64{}
		out = append(out, j)
	}
	return out, rows.Err()
}
