package vcs

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"
)

// gitProvider works with ANY git server through the git CLI — self-hosted
// git, cgit, Azure Repos, a provider Forge has no API integration for. It
// reads files and diffs with shallow, blob-less fetches (only the commits and
// trees needed, never the whole history). There is no web UI or status API.
// It is also the fallback when a host's own API errors or truncates a diff.
type gitProvider struct{}

func init() { Register(gitProvider{}) }

func (gitProvider) Name() string { return "git" }

const gitTimeout = 90 * time.Second

// authCloneURL embeds the token the way the repo's host expects.
func authCloneURL(repo RepoRef) string {
	u, err := url.Parse(repo.CloneURL)
	if err != nil || repo.Token == "" || !strings.HasPrefix(u.Scheme, "http") {
		return repo.CloneURL
	}
	host := Lookup(repo.Provider)
	if host.Name() == "git" {
		u.User = url.UserPassword("x-access-token", repo.Token)
	} else if ui := host.CloneUser(repo.Token); ui != nil {
		u.User = ui
	}
	return u.String()
}

// gitRepo is a throwaway bare repository with the remote configured as a
// partial-clone promisor, so blobs are fetched lazily and only on demand.
type gitRepo struct {
	dir    string
	secret string
}

func newGitRepo(ctx context.Context, repo RepoRef) (*gitRepo, error) {
	if !gitReachable(repo.CloneURL) {
		return nil, fmt.Errorf("vcs: %s has no clone URL git can fetch", repo.Repo)
	}
	dir, err := os.MkdirTemp("", "forge-git-*")
	if err != nil {
		return nil, err
	}
	g := &gitRepo{dir: dir, secret: repo.Token}
	for _, args := range [][]string{
		{"init", "--bare", "-q"},
		{"remote", "add", "origin", authCloneURL(repo)},
		{"config", "remote.origin.promisor", "true"},
		{"config", "remote.origin.partialclonefilter", "blob:none"},
	} {
		if _, err := g.run(ctx, args...); err != nil {
			g.close()
			return nil, err
		}
	}
	return g, nil
}

func (g *gitRepo) close() { os.RemoveAll(g.dir) }

func (g *gitRepo) run(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = g.dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=/bin/false")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if g.secret != "" {
			msg = strings.ReplaceAll(msg, g.secret, "***")
		}
		return out, fmt.Errorf("git %s: %s", args[0], msg)
	}
	return out, nil
}

// fetch brings in one commit (by sha) or branch tip (by name), shallow and
// without blobs.
func (g *gitRepo) fetch(ctx context.Context, rev string) (string, error) {
	spec := rev
	if !looksLikeSHA(rev) {
		spec = "refs/heads/" + strings.TrimPrefix(rev, "refs/heads/")
	}
	if _, err := g.run(ctx, "fetch", "-q", "--depth=1", "--filter=blob:none", "origin", spec); err != nil {
		return "", err
	}
	out, err := g.run(ctx, "rev-parse", "FETCH_HEAD")
	return strings.TrimSpace(string(out)), err
}

func looksLikeSHA(s string) bool {
	if len(s) < 7 || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

func (gitProvider) FetchFile(ctx context.Context, _ *Fetcher, repo RepoRef, sha, path string) ([]byte, bool, error) {
	g, err := newGitRepo(ctx, repo)
	if err != nil {
		return nil, false, err
	}
	defer g.close()
	commit, err := g.fetch(ctx, sha)
	if err != nil {
		return nil, false, err
	}
	out, err := g.run(ctx, "show", commit+":"+strings.TrimPrefix(path, "/"))
	if err != nil {
		if strings.Contains(err.Error(), "does not exist") || strings.Contains(err.Error(), "exists on disk, but not in") {
			return nil, false, nil
		}
		return nil, false, err
	}
	return out, true, nil
}

// ChangedFiles diffs the two trees directly (two-dot). When base is a branch
// this also lists files changed on the base since the branch point, which
// only ever makes more jobs run — the safe direction.
func (gitProvider) ChangedFiles(ctx context.Context, _ *Fetcher, repo RepoRef, base, head string) ([]string, bool, error) {
	g, err := newGitRepo(ctx, repo)
	if err != nil {
		return nil, false, err
	}
	defer g.close()
	b, err := g.fetch(ctx, base)
	if err != nil {
		return nil, false, err
	}
	h, err := g.fetch(ctx, head)
	if err != nil {
		return nil, false, err
	}
	out, err := g.run(ctx, "diff", "--name-only", "--no-renames", b, h)
	if err != nil {
		return nil, false, err
	}
	files := []string{}
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l != "" {
			files = append(files, l)
		}
	}
	return files, true, nil
}

func (gitProvider) StatusRequest(*Poster, PostRequest) (*StatusCall, error) {
	return nil, ErrNoStatusAPI
}

func (gitProvider) FileURL(RepoRef, string, string) (string, bool) { return "", false }

func (gitProvider) CloneUser(token string) *url.Userinfo {
	return url.UserPassword("x-access-token", token)
}

// gitReachable reports whether a clone URL can be fetched with plain git:
// http(s), ssh (URL or scp-style user@host:path), git:// or file://. Helper
// schemes such as codecommit:: are not.
func gitReachable(cloneURL string) bool {
	u := strings.TrimSpace(cloneURL)
	for _, p := range []string{"https://", "http://", "ssh://", "git://", "file://"} {
		if strings.HasPrefix(u, p) {
			return true
		}
	}
	return strings.Contains(u, "@") && strings.Contains(u, ":") && !strings.Contains(u, "::")
}

// gitAvailable reports whether the git CLI can be used for the fallback.
func gitAvailable() bool {
	_, err := exec.LookPath("git")
	return err == nil
}
