package store

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

// RegisterRepo stores/updates the VCS connection for a repo. An empty token
// on update keeps the existing token (so edits don't require re-entering it).
func (s *Store) RegisterRepo(ctx context.Context, r proto.RepoRegistration) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO repo_registry (repo, provider, clone_url, token, default_branch)
		 VALUES ($1,$2,$3,$4,$5)
		 ON CONFLICT (repo) DO UPDATE SET
		   provider = EXCLUDED.provider,
		   clone_url = EXCLUDED.clone_url,
		   token = CASE WHEN EXCLUDED.token = '' THEN repo_registry.token ELSE EXCLUDED.token END,
		   default_branch = EXCLUDED.default_branch`,
		r.Repo, r.Provider, r.CloneURL, r.Token, r.DefaultBranch)
	return err
}

// ListRegisteredRepos returns registrations with the token replaced by a
// has-token flag.
func (s *Store) ListRegisteredRepos(ctx context.Context) ([]proto.RepoRegistration, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT repo, provider, clone_url, token <> '', default_branch, created_at
		 FROM repo_registry ORDER BY repo`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []proto.RepoRegistration{}
	for rows.Next() {
		var r proto.RepoRegistration
		if err := rows.Scan(&r.Repo, &r.Provider, &r.CloneURL, &r.HasToken,
			&r.DefaultBranch, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ResolveRef resolves a branch/tag name to its tip SHA via git ls-remote
// against the registered clone URL. Returns ErrNotFound when the repo isn't
// registered; token is stripped from any error text.
func (s *Store) ResolveRef(ctx context.Context, repo, ref string) (string, error) {
	cloneURL, token, err := s.cloneAuth(ctx, repo)
	if err != nil {
		return "", err
	}
	if cloneURL == "" {
		return "", ErrNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "ls-remote", cloneURL,
		"refs/heads/"+ref, "refs/tags/"+ref)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := string(out)
		if token != "" {
			msg = strings.ReplaceAll(msg, token, "[REDACTED]")
		}
		return "", fmt.Errorf("ls-remote: %s", strings.TrimSpace(msg))
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return "", fmt.Errorf("ref %q not found in %s", ref, repo)
	}
	fields := strings.Fields(strings.Split(line, "\n")[0])
	return fields[0], nil
}

// cloneAuth returns the clone URL with credentials embedded (or "" when the
// repo isn't registered) plus the raw token for log redaction.
func (s *Store) cloneAuth(ctx context.Context, repo string) (cloneURL, token string, err error) {
	var provider, raw string
	err = s.pool.QueryRow(ctx,
		`SELECT provider, clone_url, token FROM repo_registry WHERE repo=$1`, repo).
		Scan(&provider, &raw, &token)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	if token == "" {
		return raw, "", nil
	}
	u, perr := url.Parse(raw)
	if perr != nil || !strings.HasPrefix(u.Scheme, "http") {
		return raw, token, nil
	}
	switch provider {
	case "bitbucket":
		u.User = url.UserPassword("x-token-auth", token)
	default: // github and generic HTTPS token auth
		u.User = url.UserPassword("x-access-token", token)
	}
	return u.String(), token, nil
}
