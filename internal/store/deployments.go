package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

// EnvBoardRow is the pure-DB portion of one environments-board card: the latest
// successful deployment for an environment and its total deployment count. The
// drift indicator is derived by the API layer (it needs a network ref-tip
// resolve, so it is kept out of the store query path).
type EnvBoardRow struct {
	Environment string
	Current     proto.Deployment
	Count       int
}

// EnvironmentsForRepo returns one row per environment the repo has deployed to,
// each with its latest successful deployment and the total number of successful
// deployments to that environment, ordered by environment name.
func (s *Store) EnvironmentsForRepo(ctx context.Context, repo string) ([]EnvBoardRow, error) {
	// Latest successful deployment per environment (DISTINCT ON keeps the first
	// row per environment under the ORDER BY, i.e. the highest id).
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT ON (environment)
		        environment, id, repo, sha, ref, pipeline_id, job_id, deployed_by, deployed_at, status
		 FROM deployments
		 WHERE repo=$1 AND status='success'
		 ORDER BY environment, id DESC`, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byEnv := map[string]*EnvBoardRow{}
	order := []string{}
	for rows.Next() {
		var d proto.Deployment
		if err := rows.Scan(&d.Environment, &d.ID, &d.Repo, &d.SHA, &d.Ref,
			&d.PipelineID, &d.JobID, &d.DeployedBy, &d.DeployedAt, &d.Status); err != nil {
			return nil, err
		}
		byEnv[d.Environment] = &EnvBoardRow{Environment: d.Environment, Current: d}
		order = append(order, d.Environment)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Per-environment success counts.
	crows, err := s.pool.Query(ctx,
		`SELECT environment, count(*) FROM deployments
		 WHERE repo=$1 AND status='success' GROUP BY environment`, repo)
	if err != nil {
		return nil, err
	}
	defer crows.Close()
	for crows.Next() {
		var env string
		var n int
		if err := crows.Scan(&env, &n); err != nil {
			return nil, err
		}
		if r, ok := byEnv[env]; ok {
			r.Count = n
		}
	}
	if err := crows.Err(); err != nil {
		return nil, err
	}

	out := make([]EnvBoardRow, 0, len(order))
	for _, env := range order {
		out = append(out, *byEnv[env])
	}
	return out, nil
}

// ListDeployments returns one page of an environment's deployment history,
// newest first, plus the total count for pagination. limit/offset are assumed
// clamped by the caller.
func (s *Store) ListDeployments(ctx context.Context, repo, env string, limit, offset int) ([]proto.Deployment, int, error) {
	var total int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM deployments WHERE repo=$1 AND environment=$2`,
		repo, env).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, repo, environment, sha, ref, pipeline_id, job_id, deployed_by, deployed_at, status
		 FROM deployments
		 WHERE repo=$1 AND environment=$2
		 ORDER BY id DESC LIMIT $3 OFFSET $4`, repo, env, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []proto.Deployment{}
	for rows.Next() {
		var d proto.Deployment
		if err := rows.Scan(&d.ID, &d.Repo, &d.Environment, &d.SHA, &d.Ref,
			&d.PipelineID, &d.JobID, &d.DeployedBy, &d.DeployedAt, &d.Status); err != nil {
			return nil, 0, err
		}
		out = append(out, d)
	}
	return out, total, rows.Err()
}

// RollbackTarget resolves the (sha, ref) to redeploy for a rollback. Exactly one
// selector is used: pipelineID (> 0) takes precedence, else sha. The target must
// be a prior deployment of this repo+environment so its ref is known — ErrNotFound
// otherwise.
func (s *Store) RollbackTarget(ctx context.Context, repo, env string, pipelineID int64, sha string) (targetSHA, targetRef string, err error) {
	if pipelineID > 0 {
		err = s.pool.QueryRow(ctx,
			`SELECT sha, ref FROM deployments
			 WHERE repo=$1 AND environment=$2 AND pipeline_id=$3
			 ORDER BY id DESC LIMIT 1`, repo, env, pipelineID).Scan(&targetSHA, &targetRef)
	} else if sha != "" {
		err = s.pool.QueryRow(ctx,
			`SELECT sha, ref FROM deployments
			 WHERE repo=$1 AND environment=$2 AND sha=$3
			 ORDER BY id DESC LIMIT 1`, repo, env, sha).Scan(&targetSHA, &targetRef)
	} else {
		return "", "", errors.New("one of to_pipeline_id or to_sha is required")
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", ErrNotFound
	}
	return targetSHA, targetRef, err
}

// ---- deploy freezes ----

// IsFrozen reports whether an active deploy-freeze window currently covers the
// (repo, environment) — a repo-specific or global (repo="") freeze.
func (s *Store) IsFrozen(ctx context.Context, repo, env string) (bool, error) {
	var frozen bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM deploy_freezes
		   WHERE environment=$2 AND repo IN ('', $1)
		     AND now() >= starts_at AND now() < ends_at)`, repo, env).Scan(&frozen)
	return frozen, err
}

// CreateFreeze inserts a deploy-freeze window and returns it.
func (s *Store) CreateFreeze(ctx context.Context, f proto.DeployFreeze) (*proto.DeployFreeze, error) {
	err := s.pool.QueryRow(ctx,
		`INSERT INTO deploy_freezes (repo, environment, starts_at, ends_at, reason)
		 VALUES ($1,$2,$3,$4,$5) RETURNING id, created_at`,
		f.Repo, f.Environment, f.StartsAt, f.EndsAt, f.Reason).Scan(&f.ID, &f.CreatedAt)
	return &f, err
}

// ListFreezes returns freeze windows for a repo (including global repo="" ones),
// or all freezes when repo is empty, newest first.
func (s *Store) ListFreezes(ctx context.Context, repo string) ([]proto.DeployFreeze, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, repo, environment, starts_at, ends_at, reason, created_at
		 FROM deploy_freezes
		 WHERE ($1 = '' OR repo IN ('', $1))
		 ORDER BY id DESC`, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []proto.DeployFreeze{}
	for rows.Next() {
		var f proto.DeployFreeze
		if err := rows.Scan(&f.ID, &f.Repo, &f.Environment, &f.StartsAt, &f.EndsAt,
			&f.Reason, &f.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// DeleteFreeze removes a freeze window by id. ErrNotFound when it does not exist.
func (s *Store) DeleteFreeze(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM deploy_freezes WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
