package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

// PromotionSource is what a promotion redeploys: an earlier successful
// deployment, the exact config its pipeline ran, and that pipeline's
// successful jobs (by name), whose artifacts and dotenv the promoted deploy
// jobs reuse instead of rebuilding.
type PromotionSource struct {
	Deployment     proto.Deployment
	Config         string
	PipelineSource string
	SucceededJobs  map[string]int64
}

// GetPromotionSource loads deployment id (> 0), or else the latest successful
// deployment of fromEnv, for repo.
func (s *Store) GetPromotionSource(ctx context.Context, repo, fromEnv string, id int64) (*PromotionSource, error) {
	var src PromotionSource
	d := &src.Deployment
	q := `SELECT d.id, d.repo, d.environment, d.sha, d.ref, d.pipeline_id, d.job_id, d.deployed_by, d.deployed_at,
	             d.status, d.url, d.version, d.digest, d.promoted_from, p.config_yaml, p.source
	      FROM deployments d JOIN pipelines p ON p.id = d.pipeline_id
	      WHERE d.repo = $1 AND d.status = 'success' AND `
	args := []any{repo}
	if id > 0 {
		q += `d.id = $2`
		args = append(args, id)
	} else {
		if fromEnv == "" {
			return nil, errors.New("one of deployment_id or from_environment is required")
		}
		q += `d.environment = $2 ORDER BY d.id DESC LIMIT 1`
		args = append(args, fromEnv)
	}
	err := s.pool.QueryRow(ctx, q, args...).Scan(&d.ID, &d.Repo, &d.Environment, &d.SHA, &d.Ref, &d.PipelineID,
		&d.JobID, &d.DeployedBy, &d.DeployedAt, &d.Status, &d.URL, &d.Version, &d.Digest, &d.PromotedFrom,
		&src.Config, &src.PipelineSource)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	// A promoted deployment's own pipeline only re-ran the deploy jobs; the
	// build lives in the pipeline it was promoted from. Follow the chain so
	// staging → production reuses the original build.
	pipelineID := d.PipelineID
	for hop, from := 0, d.PromotedFrom; from != nil && hop < 20; hop++ {
		var next *int64
		var pid int64
		var cfg, source string
		if err := s.pool.QueryRow(ctx,
			`SELECT d.pipeline_id, d.promoted_from, p.config_yaml, p.source FROM deployments d
			 JOIN pipelines p ON p.id = d.pipeline_id WHERE d.id = $1`, *from).Scan(&pid, &next, &cfg, &source); err != nil {
			break
		}
		pipelineID, from = pid, next
		src.Config, src.PipelineSource = cfg, source
	}
	src.SucceededJobs = map[string]int64{}
	rows, err := s.pool.Query(ctx,
		`SELECT name, id FROM jobs WHERE pipeline_id = $1 AND status = 'success' AND NOT superseded`, pipelineID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var jid int64
		if err := rows.Scan(&name, &jid); err != nil {
			return nil, err
		}
		src.SucceededJobs[name] = jid
	}
	return &src, rows.Err()
}

// CancelSupersededDeploys cancels waiting deploy jobs (created, pending or
// blocked — never one already running) to environments registered with
// cancel_superseded, when a job from a newer pipeline of the same repo has
// started or succeeded deploying to the same environment. Their dependents
// are cascaded by CancelDeadJobs as for any cancel.
func (s *Store) CancelSupersededDeploys(ctx context.Context) (int64, error) {
	policy, err := s.GetEnvPolicy(ctx)
	if err != nil {
		return 0, err
	}
	var envs []string
	for _, e := range policy.Environments {
		if e.CancelSuperseded {
			envs = append(envs, e.Name)
		}
	}
	if len(envs) == 0 {
		return 0, nil
	}
	tag, err := s.pool.Exec(ctx,
		`WITH stale AS (
		   SELECT j.id, (SELECT max(p2.id) FROM jobs j2 JOIN pipelines p2 ON p2.id = j2.pipeline_id
		                 WHERE p2.repo = p.repo AND j2.environment = j.environment AND p2.id > p.id
		                   AND NOT j2.superseded AND j2.status IN ('running', 'success')) AS newer
		   FROM jobs j JOIN pipelines p ON p.id = j.pipeline_id
		   WHERE j.environment = ANY($1) AND j.status IN ('created', 'pending', 'blocked') AND NOT j.superseded)
		 UPDATE jobs j SET status = 'canceled', finished_at = now(),
		        status_reason = 'superseded: pipeline #' || stale.newer || ' has a newer deploy to ' || j.environment
		 FROM stale WHERE j.id = stale.id AND stale.newer IS NOT NULL`, envs)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
