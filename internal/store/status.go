package store

import (
	"context"
	"encoding/json"
)

// StatusCandidate is one pipeline whose current commit-status phase has not yet
// been posted to the origin VCS. The scheduler claims and delivers it.
type StatusCandidate struct {
	PipelineID int64
	Repo       string
	Ref        string
	SHA        string
	Provider   string // github | bitbucket (never "other" — filtered out)
	Status     string // the phase to post (see statusForPost)
}

// statusForPost derives the commit-status phase for a pipeline from its jobs'
// statuses. It differs from deriveStatus (which drives the UI) in one way: a
// freshly-created pipeline whose jobs are all still "created" is reported as
// "pending" here rather than collapsed into "running", so the commit shows a
// distinct queued -> in-progress -> final progression. Both "pending" and
// "running" map to the provider "in progress" state, so the external commit
// view is consistent regardless.
//
// Precedence mirrors deriveStatus: any failure fails the pipeline, then a
// cancellation, then in-flight work, then a pending approval gate.
func statusForPost(statuses []string) string {
	has := map[string]bool{}
	for _, st := range statuses {
		has[st] = true
	}
	switch {
	case len(statuses) == 0:
		return "pending"
	case has["failed"]:
		return "failed"
	case has["canceled"]:
		return "canceled"
	case has["running"], has["pending"]:
		return "running"
	case has["blocked"]:
		return "blocked"
	case has["created"]:
		return "pending"
	default:
		return "success"
	}
}

// PipelinesPendingStatusPost returns pipelines whose current commit-status phase
// has not yet been posted. It considers only pipelines connected to a postable
// VCS (provider github/bitbucket with a token) and skips any pipeline that has
// already had a terminal status posted (so finished pipelines drop out of the
// scan for good). The current phase is derived in Go via statusForPost and
// compared against the statuses already posted for that pipeline.
func (s *Store) PipelinesPendingStatusPost(ctx context.Context) ([]StatusCandidate, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT p.id, p.repo, p.ref, p.sha, rr.provider,
		        COALESCE(json_agg(DISTINCT j.status) FILTER (WHERE j.id IS NOT NULL), '[]'),
		        COALESCE((SELECT array_agg(sp.status)
		                  FROM pipeline_status_posts sp WHERE sp.pipeline_id = p.id), '{}'::text[])
		 FROM pipelines p
		 JOIN repo_registry rr ON rr.repo = p.repo
		      AND rr.provider IN ('github','bitbucket')
		      AND (rr.token <> ''
		           OR (rr.github_app_id <> '' AND rr.github_app_installation_id <> ''
		               AND rr.github_app_private_key <> ''))
		 LEFT JOIN jobs j ON j.pipeline_id = p.id
		 WHERE NOT EXISTS (
		   SELECT 1 FROM pipeline_status_posts sp
		   WHERE sp.pipeline_id = p.id AND sp.status IN ('success','failed','canceled'))
		 GROUP BY p.id, rr.provider
		 ORDER BY p.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []StatusCandidate{}
	for rows.Next() {
		var c StatusCandidate
		var statusesRaw []byte
		var posted []string
		if err := rows.Scan(&c.PipelineID, &c.Repo, &c.Ref, &c.SHA, &c.Provider, &statusesRaw, &posted); err != nil {
			return nil, err
		}
		var statuses []string
		if err := json.Unmarshal(statusesRaw, &statuses); err != nil {
			return nil, err
		}
		cur := statusForPost(statuses)
		alreadyPosted := false
		for _, p := range posted {
			if p == cur {
				alreadyPosted = true
				break
			}
		}
		if alreadyPosted {
			continue
		}
		c.Status = cur
		out = append(out, c)
	}
	return out, rows.Err()
}

// ClaimStatusPost records that (pipeline_id, status) is being posted and reports
// whether this call won the claim. A false return means another tick/instance
// already claimed it — the caller must not post. The row is the durable dedup
// key; ReleaseStatusPost undoes a claim so a transient failure can be retried.
func (s *Store) ClaimStatusPost(ctx context.Context, pipelineID int64, status string) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`INSERT INTO pipeline_status_posts (pipeline_id, status) VALUES ($1,$2)
		 ON CONFLICT (pipeline_id, status) DO NOTHING`, pipelineID, status)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ReleaseStatusPost removes a claim so the status can be re-attempted on a later
// scheduler tick. Used only after a transient delivery failure; a permanent
// failure keeps the claim so a misconfigured repo is not hammered.
func (s *Store) ReleaseStatusPost(ctx context.Context, pipelineID int64, status string) error {
	_, err := s.pool.Exec(ctx,
		`DELETE FROM pipeline_status_posts WHERE pipeline_id=$1 AND status=$2`, pipelineID, status)
	return err
}

// RepoStatusTarget returns a repo's VCS provider and a ready-to-use token for
// making commit-status API calls. The token is a static PAT or a freshly-minted
// (and cached) GitHub App installation token, resolved through the same path as
// cloneAuth (see resolveRepoAuth). ok is false when the repo is not registered.
// The token is used only for outbound VCS API calls (Authorization header) — it
// is never returned over Forge's own HTTP API and never logged.
func (s *Store) RepoStatusTarget(ctx context.Context, repo string) (provider, token string, ok bool, err error) {
	provider, _, token, found, err := s.resolveRepoAuth(ctx, repo)
	if err != nil {
		return "", "", false, err
	}
	if !found {
		return "", "", false, nil
	}
	return provider, token, true, nil
}
