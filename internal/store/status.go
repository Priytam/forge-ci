package store

import (
	"context"
	"encoding/json"

	"github.com/priytamjeepandey/forge-ci/internal/vcs"
)

// StatusCandidate is one pipeline whose current commit-status phase has not yet
// been posted to the origin VCS. The scheduler claims and delivers it.
type StatusCandidate struct {
	PipelineID int64
	Repo       string
	Ref        string
	SHA        string
	Provider   string // github | bitbucket | codecommit (never "other" — filtered out)
	Status     string // the phase to post (see statusForPost)

	// CodeCommit only: it has no commit-status API, so the result goes to the
	// originating pull request as a comment. MRIID is non-empty for exactly the
	// runs that have somewhere to report to.
	MRIID     string
	MRBaseSHA string
}

// isTerminalStatus reports whether a phase is a pipeline's final word.
// CodeCommit posts only these: a comment per phase transition would spam the
// pull request, where a commit status is simply overwritten in place.
func isTerminalStatus(status string) bool {
	switch status {
	case "success", "failed", "canceled":
		return true
	}
	return false
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

// PipelinesPendingStatusPost returns pipelines whose current status phase has
// not yet been reported to the origin. It considers only pipelines with
// somewhere to report to and skips any pipeline that has already had a terminal
// status posted (so finished pipelines drop out of the scan for good). The
// current phase is derived in Go via statusForPost and compared against the
// statuses already posted for that pipeline.
//
// "Somewhere to report to" differs by provider. GitHub and Bitbucket need a
// credential — a token or a complete App config — because they write through an
// authenticated status API. CodeCommit needs no credential at all (the call is
// IAM-signed) but does need a PULL REQUEST, since a comment is the only channel
// it has; a CodeCommit push run is therefore never a candidate, which is exactly
// the "non-PR runs post nothing" rule. CodeCommit candidates are further limited
// to terminal phases, so a pull request gets one comment rather than a running
// commentary.
func (s *Store) PipelinesPendingStatusPost(ctx context.Context) ([]StatusCandidate, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT p.id, p.repo, p.ref, p.sha, rr.provider, p.mr_iid, p.mr_base_sha,
		        COALESCE(json_agg(DISTINCT CASE WHEN j.status='failed' AND j.allow_failure THEN 'success' ELSE j.status END) FILTER (WHERE j.id IS NOT NULL), '[]'),
		        COALESCE((SELECT array_agg(sp.status)
		                  FROM pipeline_status_posts sp WHERE sp.pipeline_id = p.id), '{}'::text[])
		 FROM pipelines p
		 JOIN repo_registry rr ON rr.repo = p.repo
		      AND (
		        (rr.provider IN ('github','bitbucket')
		         AND (rr.token <> ''
		              OR (rr.github_app_id <> '' AND rr.github_app_installation_id <> ''
		                  AND rr.github_app_private_key <> '')))
		        OR (rr.provider = 'codecommit' AND p.mr_iid <> '')
		      )
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
		if err := rows.Scan(&c.PipelineID, &c.Repo, &c.Ref, &c.SHA, &c.Provider,
			&c.MRIID, &c.MRBaseSHA, &statusesRaw, &posted); err != nil {
			return nil, err
		}
		var statuses []string
		if err := json.Unmarshal(statusesRaw, &statuses); err != nil {
			return nil, err
		}
		cur := statusForPost(statuses)
		// A comment cannot be edited in place the way a commit status can, so
		// CodeCommit waits for the run's final word instead of narrating it.
		if c.Provider == "codecommit" && !isTerminalStatus(cur) {
			continue
		}
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
	conn, found, err := s.resolveRepoAuth(ctx, repo)
	if err != nil {
		return "", "", false, err
	}
	if !found {
		return "", "", false, nil
	}
	return conn.Provider, conn.Token, true, nil
}

// CodeCommitStatusTarget resolves where a CodeCommit pull-request comment
// should go. ok is false when the repo is not registered, is not a CodeCommit
// connection, or does not resolve to a region and repository name. No credential
// is returned because none exists: the comment call is IAM-signed.
func (s *Store) CodeCommitStatusTarget(ctx context.Context, repo string) (cc vcs.CodeCommitRepo, roleARN string, ok bool, err error) {
	conn, found, err := s.resolveRepoAuth(ctx, repo)
	if err != nil || !found || conn.Provider != "codecommit" {
		return vcs.CodeCommitRepo{}, "", false, err
	}
	cc, ok = conn.codeCommitRepo()
	return cc, conn.AWS.RoleARN, ok, nil
}
