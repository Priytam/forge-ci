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

	"github.com/priytamjeepandey/forge-ci/internal/githubapp"
	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

// RegisterRepo stores/updates the VCS connection for a repo. Both secrets — the
// static token and the GitHub App private key — are preserved on update when
// sent empty (Encrypt is a no-op on ""), so edits that keep the existing secret
// don't require re-entering it. app_id/installation_id are not secret and are
// overwritten from the request (the client resends them, they're in GET).
func (s *Store) RegisterRepo(ctx context.Context, r proto.RepoRegistration) error {
	encToken, err := s.cipher.Encrypt(r.Token)
	if err != nil {
		return err
	}
	encKey, err := s.cipher.Encrypt(r.GitHubAppPrivateKey)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx,
		`INSERT INTO repo_registry
		     (repo, provider, clone_url, token, default_branch,
		      github_app_id, github_app_private_key, github_app_installation_id)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		 ON CONFLICT (repo) DO UPDATE SET
		   provider = EXCLUDED.provider,
		   clone_url = EXCLUDED.clone_url,
		   token = CASE WHEN EXCLUDED.token = '' THEN repo_registry.token ELSE EXCLUDED.token END,
		   default_branch = EXCLUDED.default_branch,
		   github_app_id = EXCLUDED.github_app_id,
		   github_app_private_key = CASE WHEN EXCLUDED.github_app_private_key = ''
		       THEN repo_registry.github_app_private_key ELSE EXCLUDED.github_app_private_key END,
		   github_app_installation_id = EXCLUDED.github_app_installation_id`,
		r.Repo, r.Provider, r.CloneURL, encToken, r.DefaultBranch,
		r.GitHubAppID, encKey, r.GitHubInstallationID)
	return err
}

// ListRegisteredRepos returns registrations with secrets replaced by has-* flags.
// The token and the GitHub App private key are NEVER returned; app_id and
// installation_id (not secret) are.
func (s *Store) ListRegisteredRepos(ctx context.Context) ([]proto.RepoRegistration, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT repo, provider, clone_url, token <> '', default_branch, created_at,
		        github_app_id, github_app_installation_id, github_app_id <> ''
		 FROM repo_registry ORDER BY repo`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []proto.RepoRegistration{}
	for rows.Next() {
		var r proto.RepoRegistration
		if err := rows.Scan(&r.Repo, &r.Provider, &r.CloneURL, &r.HasToken,
			&r.DefaultBranch, &r.CreatedAt,
			&r.GitHubAppID, &r.GitHubInstallationID, &r.HasGitHubApp); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// resolveRepoAuth loads a repo's connection and returns its provider, raw clone
// URL, and a ready-to-use plaintext token — a static PAT, or a freshly minted
// (and cached) GitHub App installation token when the repo is App-authed. App
// auth takes precedence when configured. found is false when the repo isn't
// registered; token is "" for a public repo with neither a PAT nor an App. The
// token is returned only for building an authed clone URL / Authorization header
// — never logged, never returned by Forge's own API.
func (s *Store) resolveRepoAuth(ctx context.Context, repo string) (provider, cloneURL, token string, found bool, err error) {
	var rawURL, encToken, appID, encKey, installID string
	err = s.pool.QueryRow(ctx,
		`SELECT provider, clone_url, token,
		        github_app_id, github_app_private_key, github_app_installation_id
		 FROM repo_registry WHERE repo=$1`, repo).
		Scan(&provider, &rawURL, &encToken, &appID, &encKey, &installID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", "", false, nil
	}
	if err != nil {
		return "", "", "", false, err
	}
	// GitHub App auth takes precedence when fully configured.
	if appID != "" && installID != "" && encKey != "" {
		keyPEM, derr := s.cipher.Decrypt(encKey)
		if derr != nil {
			return "", "", "", false, derr
		}
		tok, _, merr := s.appMinter.Token(ctx, githubapp.Config{
			AppID: appID, PrivateKeyPEM: keyPEM, InstallationID: installID,
		})
		if merr != nil {
			return "", "", "", false, merr
		}
		return provider, rawURL, tok, true, nil
	}
	tok, derr := s.cipher.Decrypt(encToken)
	if derr != nil {
		return "", "", "", false, derr
	}
	return provider, rawURL, tok, true, nil
}

// AppTokenForValidation mints a GitHub App installation token so the
// registration handler can verify access (git ls-remote) before saving. It uses
// keyPEM when supplied (a new/rotated key in the request), otherwise the key
// already stored for repo (a metadata-only edit). Errors never contain the key.
func (s *Store) AppTokenForValidation(ctx context.Context, repo, appID, keyPEM, installID string) (string, error) {
	if keyPEM == "" {
		var enc string
		err := s.pool.QueryRow(ctx,
			`SELECT github_app_private_key FROM repo_registry WHERE repo=$1`, repo).Scan(&enc)
		if errors.Is(err, pgx.ErrNoRows) || enc == "" {
			return "", errors.New("no stored private key — supply github_app_private_key")
		}
		if err != nil {
			return "", err
		}
		keyPEM, err = s.cipher.Decrypt(enc)
		if err != nil {
			return "", err
		}
	}
	tok, _, err := s.appMinter.Token(ctx, githubapp.Config{
		AppID: appID, PrivateKeyPEM: keyPEM, InstallationID: installID,
	})
	return tok, err
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
// repo isn't registered) plus the raw token for log redaction. The token is a
// static PAT or a freshly-minted GitHub App installation token, transparently
// (see resolveRepoAuth) — the runner is handed a ready-to-use authed URL either
// way.
func (s *Store) cloneAuth(ctx context.Context, repo string) (cloneURL, token string, err error) {
	provider, raw, tok, found, err := s.resolveRepoAuth(ctx, repo)
	if err != nil {
		return "", "", err
	}
	if !found {
		return "", "", nil
	}
	if tok == "" {
		return raw, "", nil
	}
	u, perr := url.Parse(raw)
	if perr != nil || !strings.HasPrefix(u.Scheme, "http") {
		return raw, tok, nil
	}
	switch provider {
	case "bitbucket":
		u.User = url.UserPassword("x-token-auth", tok)
	default: // github and generic HTTPS token auth
		u.User = url.UserPassword("x-access-token", tok)
	}
	return u.String(), tok, nil
}
