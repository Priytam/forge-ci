package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/priytamjeepandey/forge-ci/internal/githubapp"
	"github.com/priytamjeepandey/forge-ci/internal/proto"
	"github.com/priytamjeepandey/forge-ci/internal/vcs"
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
	configSource := r.ConfigSource
	if configSource == "" {
		configSource = "repo" // default: config rides in the commit/PR
	}
	// The AWS columns hold nothing secret, so unlike token / private key they are
	// overwritten from the request rather than preserved when empty — clearing a
	// role_arn must actually clear it.
	_, err = s.pool.Exec(ctx,
		`INSERT INTO repo_registry
		     (repo, provider, clone_url, token, default_branch,
		      github_app_id, github_app_private_key, github_app_installation_id,
		      config_source, config_path,
		      aws_region, aws_profile, aws_role_arn)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		 ON CONFLICT (repo) DO UPDATE SET
		   provider = EXCLUDED.provider,
		   clone_url = EXCLUDED.clone_url,
		   token = CASE WHEN EXCLUDED.token = '' THEN repo_registry.token ELSE EXCLUDED.token END,
		   default_branch = EXCLUDED.default_branch,
		   github_app_id = EXCLUDED.github_app_id,
		   github_app_private_key = CASE WHEN EXCLUDED.github_app_private_key = ''
		       THEN repo_registry.github_app_private_key ELSE EXCLUDED.github_app_private_key END,
		   github_app_installation_id = EXCLUDED.github_app_installation_id,
		   config_source = EXCLUDED.config_source,
		   config_path = EXCLUDED.config_path,
		   aws_region = EXCLUDED.aws_region,
		   aws_profile = EXCLUDED.aws_profile,
		   aws_role_arn = EXCLUDED.aws_role_arn`,
		r.Repo, r.Provider, r.CloneURL, encToken, r.DefaultBranch,
		r.GitHubAppID, encKey, r.GitHubInstallationID,
		configSource, r.ConfigPath,
		r.AWSRegion, r.AWSProfile, r.AWSRoleARN)
	return err
}

// ListRegisteredRepos returns registrations with secrets replaced by has-* flags.
// The token and the GitHub App private key are NEVER returned; app_id and
// installation_id (not secret) are, as are the AWS region/profile/role — a
// CodeCommit connection stores no secret at all.
func (s *Store) ListRegisteredRepos(ctx context.Context) ([]proto.RepoRegistration, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT repo, provider, clone_url, token <> '', default_branch, created_at,
		        github_app_id, github_app_installation_id, github_app_id <> '',
		        config_source, config_path,
		        aws_region, aws_profile, aws_role_arn
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
			&r.GitHubAppID, &r.GitHubInstallationID, &r.HasGitHubApp,
			&r.ConfigSource, &r.ConfigPath,
			&r.AWSRegion, &r.AWSProfile, &r.AWSRoleARN); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// repoConn is a repo's resolved VCS connection: everything needed to reach the
// origin, with any credential already decrypted or minted.
type repoConn struct {
	Provider string // github | bitbucket | codecommit | other
	CloneURL string // as registered, without embedded credentials
	Token    string // PAT or minted App token; always "" for codecommit
	AWS      awsConn
}

// awsConn is the AWS context for a CodeCommit connection. None of it is secret:
// the identity itself comes from the control plane's credential chain at call
// time, so these three only say WHERE to look and WHICH role to wear.
type awsConn struct {
	Region  string
	Profile string
	RoleARN string
}

// codeCommitRepo resolves the CodeCommit coordinates for this connection.
// The registered aws_region/aws_profile columns are authoritative; a connection
// registered by clone URL alone (or predating those columns) falls back to the
// region encoded in the URL, so both registration styles work.
func (c repoConn) codeCommitRepo() (vcs.CodeCommitRepo, bool) {
	parsed, ok := vcs.ParseCodeCommitURL(c.CloneURL)
	if c.AWS.Region != "" {
		parsed.Region = c.AWS.Region
		ok = ok || parsed.Name != ""
	}
	if c.AWS.Profile != "" {
		parsed.Profile = c.AWS.Profile
	}
	return parsed, ok && parsed.Region != "" && parsed.Name != ""
}

// resolveRepoAuth loads a repo's connection with a ready-to-use credential — a
// static PAT, or a freshly minted (and cached) GitHub App installation token
// when the repo is App-authed. App auth takes precedence when configured. found
// is false when the repo isn't registered; Token is "" for a public repo with
// neither a PAT nor an App, and always "" for CodeCommit, which authenticates
// with an AWS identity instead of a credential Forge holds. The token is
// returned only for building an authed clone URL / Authorization header — never
// logged, never returned by Forge's own API.
func (s *Store) resolveRepoAuth(ctx context.Context, repo string) (conn repoConn, found bool, err error) {
	var encToken, appID, encKey, installID string
	err = s.pool.QueryRow(ctx,
		`SELECT provider, clone_url, token,
		        github_app_id, github_app_private_key, github_app_installation_id,
		        aws_region, aws_profile, aws_role_arn
		 FROM repo_registry WHERE repo=$1`, repo).
		Scan(&conn.Provider, &conn.CloneURL, &encToken, &appID, &encKey, &installID,
			&conn.AWS.Region, &conn.AWS.Profile, &conn.AWS.RoleARN)
	if errors.Is(err, pgx.ErrNoRows) {
		return repoConn{}, false, nil
	}
	if err != nil {
		return repoConn{}, false, err
	}
	// CodeCommit never carries a token; skip credential resolution entirely.
	if conn.Provider == "codecommit" {
		return conn, true, nil
	}
	// GitHub App auth takes precedence when fully configured.
	if appID != "" && installID != "" && encKey != "" {
		keyPEM, derr := s.cipher.Decrypt(encKey)
		if derr != nil {
			return repoConn{}, false, derr
		}
		tok, _, merr := s.appMinter.Token(ctx, githubapp.Config{
			AppID: appID, PrivateKeyPEM: keyPEM, InstallationID: installID,
		})
		if merr != nil {
			return repoConn{}, false, merr
		}
		conn.Token = tok
		return conn, true, nil
	}
	tok, derr := s.cipher.Decrypt(encToken)
	if derr != nil {
		return repoConn{}, false, derr
	}
	conn.Token = tok
	return conn, true, nil
}

// repoConfigSource returns a repo's config source ('repo' | 'registered') and
// its config-path override. An unregistered repo defaults to ('repo', "") so the
// in-repo path is attempted (and transparently falls back when there is no
// connection to fetch through).
func (s *Store) repoConfigSource(ctx context.Context, repo string) (source, path string, err error) {
	err = s.pool.QueryRow(ctx,
		`SELECT config_source, config_path FROM repo_registry WHERE repo=$1`, repo).
		Scan(&source, &path)
	if errors.Is(err, pgx.ErrNoRows) {
		return "repo", "", nil
	}
	if err != nil {
		return "", "", err
	}
	if source == "" {
		source = "repo"
	}
	return source, path, nil
}

// ConfigFromRepo fetches the in-repo pipeline config (path, default
// .forge-ci.yml) at sha using the repo's connection auth (static PAT, minted
// GitHub App token, or — for CodeCommit — a SigV4-signed GetFile made with the
// control plane's own AWS identity). found is false (nil error) when the repo isn't registered,
// the provider has no supported contents API, or the file is absent (404) — all
// "fall back to the registered config" cases. The token is never logged.
func (s *Store) ConfigFromRepo(ctx context.Context, repo, sha, path string) (config string, found bool, err error) {
	conn, registered, err := s.resolveRepoAuth(ctx, repo)
	if err != nil {
		return "", false, err
	}
	if !registered {
		return "", false, nil
	}
	if path == "" {
		path = vcs.DefaultConfigPath
	}
	// CodeCommit takes no token: the region (from the registry, else the clone
	// URL) and the optional role are what the SigV4-signed GetFile needs, and the
	// identity behind it is the control plane's own.
	content, ok, err := s.fetcher.FetchFile(ctx, vcs.FetchRequest{
		Provider: conn.Provider, Repo: repo, SHA: sha, Path: path, Token: conn.Token,
		CloneURL: conn.CloneURL, Region: conn.AWS.Region, Profile: conn.AWS.Profile,
		RoleARN: conn.AWS.RoleARN,
	})
	if err != nil {
		return "", false, err
	}
	if !ok {
		return "", false, nil
	}
	return string(content), true, nil
}

// ResolvePipelineConfig chooses the pipeline config for a (repo, sha) event per
// the repo's config_source toggle, returning the chosen YAML, the registry
// version to stamp (nil for an in-repo config), and the source label ('repo' |
// 'registered') for CreatePipeline.
//
// Precedence when source is 'repo' (the default): fetch the in-repo
// .forge-ci.yml at sha; if present use it (version nil); if absent fall back to
// the registered config. When source is 'registered' the in-repo file is never
// fetched. ErrNotFound is returned only when neither an in-repo file nor a
// registered config exists. A hard fetch error (non-404) is logged (without the
// token) and treated as a fall-back to the registered config so a transient
// provider blip does not wholly block a pipeline that also has a registered one.
func (s *Store) ResolvePipelineConfig(ctx context.Context, repo, sha string) (config string, version *int, source string, err error) {
	cfgSource, cfgPath, err := s.repoConfigSource(ctx, repo)
	if err != nil {
		return "", nil, "", err
	}
	if cfgSource == "repo" {
		yml, found, ferr := s.ConfigFromRepo(ctx, repo, sha, cfgPath)
		if ferr != nil {
			// Never log the token; ConfigFromRepo/FetchFile already keep it out of err.
			slog.Warn("config-from-repo fetch failed, falling back to registered config",
				"repo", repo, "err", ferr)
		} else if found {
			return yml, nil, "repo", nil
		}
	}
	// Registered-config fallback (also the 'registered' toggle path).
	registered, err := s.GetRepoConfig(ctx, repo)
	if err != nil {
		return "", nil, "", err // ErrNotFound bubbles up as "no config"
	}
	var v *int
	if cur, verr := s.CurrentConfigVersion(ctx, repo); verr == nil && cur > 0 {
		v = &cur
	}
	return registered, v, "registered", nil
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
	cloneURL, token, _, err = s.cloneTarget(ctx, repo)
	return cloneURL, token, err
}

// cloneTarget is cloneAuth plus the resolved connection, for callers that need
// the provider and its AWS context to tell the runner HOW to authenticate.
func (s *Store) cloneTarget(ctx context.Context, repo string) (cloneURL, token string, conn repoConn, err error) {
	conn, found, err := s.resolveRepoAuth(ctx, repo)
	if err != nil {
		return "", "", repoConn{}, err
	}
	if !found {
		return "", "", repoConn{}, nil
	}
	// CodeCommit is handed to the runner unauthenticated on purpose: there is no
	// token to embed, and the runner signs the clone with its own IAM identity.
	// Falling through would splice a bogus x-access-token into the URL.
	if conn.Provider == "codecommit" {
		return conn.CloneURL, "", conn, nil
	}
	if conn.Token == "" {
		return conn.CloneURL, "", conn, nil
	}
	u, perr := url.Parse(conn.CloneURL)
	if perr != nil || !strings.HasPrefix(u.Scheme, "http") {
		return conn.CloneURL, conn.Token, conn, nil
	}
	switch conn.Provider {
	case "bitbucket":
		u.User = url.UserPassword("x-token-auth", conn.Token)
	default: // github and generic HTTPS token auth
		u.User = url.UserPassword("x-access-token", conn.Token)
	}
	return u.String(), conn.Token, conn, nil
}

// ValidateCodeCommitAccess proves a CodeCommit registration is usable before it
// is saved, standing in for the git ls-remote the HTTPS providers validate with.
// It resolves the same AWS identity config-from-repo will later use, so a
// registration that passes here is one whose .forge-ci.yml can actually be read.
func (s *Store) ValidateCodeCommitAccess(ctx context.Context, r proto.RepoRegistration) error {
	repo, ok := repoConn{
		Provider: r.Provider,
		CloneURL: r.CloneURL,
		AWS:      awsConn{Region: r.AWSRegion, Profile: r.AWSProfile, RoleARN: r.AWSRoleARN},
	}.codeCommitRepo()
	if !ok {
		return errors.New("could not determine the region and repository name")
	}
	return s.fetcher.CheckCodeCommitAccess(ctx, repo, r.AWSRoleARN)
}

// ConfigFileURL deep-links the pipeline config a run used, in the origin
// provider's web UI at the run's commit. It returns "" (with a nil error) when
// no correct link can be built, which the API surfaces as "no link" rather than
// an error — a missing link is a normal state, not a failure.
//
// The link is emitted ONLY for a run whose config came from the repo
// (configSource "repo"). A run that used the REGISTERED config did not run the
// file sitting in git at that commit, so linking to it would show a pipeline
// definition that is not the one that executed — the exact confusion the link
// exists to remove. Registered runs already carry their version in the config
// chip, and the config Forge holds is retrievable per version through
// /api/v1/repo-configs. (Persisting and serving each run's exact config, so
// custom runs get a faithful link too, is tracked separately.)
func (s *Store) ConfigFileURL(ctx context.Context, repo, sha, configSource string) (string, error) {
	if configSource != "repo" {
		return "", nil
	}
	conn, found, err := s.resolveRepoAuth(ctx, repo)
	if err != nil || !found {
		return "", err
	}
	_, cfgPath, err := s.repoConfigSource(ctx, repo)
	if err != nil {
		return "", err
	}
	link, ok := vcs.ConfigFileURL(vcs.ConfigLinkRequest{
		Provider: conn.Provider,
		Repo:     repo,
		SHA:      sha,
		Path:     cfgPath,
		CloneURL: conn.CloneURL,
		Region:   conn.AWS.Region,
	})
	if !ok {
		return "", nil
	}
	return link, nil
}
