package store

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/priytamjeepandey/forge-ci/internal/compiler"
	"github.com/priytamjeepandey/forge-ci/internal/vcs"
)

// PipelineChangeSet works out which files a new pipeline's commit changed,
// for rules: changes:. The comparison base is, in order:
//
//   - a merge request: its target (mrBase: the target commit or branch);
//   - the last successful pipeline on the same repo and branch;
//   - for a branch with no successful run yet: the repo's default branch
//     (GitHub/Bitbucket diff from the merge base).
//
// Only push / webhook / merge_request pipelines of connected repos get a change
// set. It returns nil — "unknown", every changes: clause matches — for other
// sources (API runs, schedules, as in GitLab), for unconnected repos, the
// default branch's first run, a provider error, or a truncated list. A nil
// change set never skips a job.
func (s *Store) PipelineChangeSet(ctx context.Context, repo, ref, sha, source, mrBase string) *compiler.ChangeSet {
	switch source {
	case compiler.SourcePush, compiler.SourceWebhook, compiler.SourceMergeRequest:
	default:
		return nil
	}
	conn, registered, err := s.resolveRepoAuth(ctx, repo)
	if err != nil || !registered {
		return nil
	}
	base := ""
	if source == compiler.SourceMergeRequest {
		base = mrBase
	}
	if base == "" {
		err := s.pool.QueryRow(ctx,
			`SELECT sha FROM pipelines WHERE repo = $1 AND ref = $2 AND status = 'success' AND sha <> $3
			 ORDER BY id DESC LIMIT 1`, repo, ref, sha).Scan(&base)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			slog.Warn("changes: previous pipeline lookup", "repo", repo, "err", err)
			return nil
		}
	}
	if base == "" {
		var def string
		_ = s.pool.QueryRow(ctx, `SELECT default_branch FROM repo_registry WHERE repo = $1`, repo).Scan(&def)
		branch := strings.TrimPrefix(ref, "refs/heads/")
		if def == "" || def == branch {
			return nil
		}
		base = def
	}
	files, complete, err := s.fetcher.ChangedFiles(ctx, vcs.CompareRequest{
		Provider: conn.Provider, Repo: repo, Base: base, Head: sha, Token: conn.Token,
		CloneURL: conn.CloneURL, Region: conn.AWS.Region, Profile: conn.AWS.Profile, RoleARN: conn.AWS.RoleARN,
	})
	if err != nil {
		slog.Warn("changes: compare failed; treating every rules: changes: as matched",
			"repo", repo, "base", base, "head", sha, "err", err)
		return nil
	}
	if !complete {
		slog.Info("changes: change list incomplete; treating every rules: changes: as matched",
			"repo", repo, "files", len(files))
		return nil
	}
	if files == nil {
		files = []string{}
	}
	return &compiler.ChangeSet{Base: base, Files: files}
}
