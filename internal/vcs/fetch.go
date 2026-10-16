package vcs

// This file implements config-from-repo: fetching a single file's contents from
// the origin VCS at an exact sha, using the connection's auth. Forge does not
// host the repo, so to let pipeline config ride in the commit/PR (the in-repo
// .forge-ci.yml) it pulls the file over the provider's contents API at the event
// sha. The token (a static PAT or a freshly-minted GitHub App installation
// token) is used only in the outbound Authorization header — never logged and
// never returned in errors. A missing file (404) is reported as (found=false,
// nil error) so callers can fall back to the registered config.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultConfigPath is the in-repo pipeline config file fetched when a repo's
// config_path override is unset.
const DefaultConfigPath = ".forge-ci.yml"

// FetchRequest is one file-contents read at an exact ref/sha. Token is a
// plaintext VCS token (PAT or minted GitHub App installation token) used only in
// the Authorization header; it is never logged.
type FetchRequest struct {
	Provider string // github | bitbucket | codecommit | other
	Repo     string // owner/name (github/bitbucket) or repository name (codecommit)
	SHA      string // the exact commit sha to read at
	Path     string // repo-relative file path (e.g. .forge-ci.yml)
	Token    string // plaintext auth; "" fetches anonymously (public repo)

	// CodeCommit only. There is no token: the call is SigV4-signed with the
	// control plane's own AWS identity. Region and Profile are authoritative when
	// set; otherwise they are read off CloneURL, so a connection registered by URL
	// alone still resolves. RoleARN, when set, is assumed before the call so one
	// Forge deployment can read repositories across accounts.
	CloneURL string
	Region   string
	Profile  string
	RoleARN  string
}

// Fetcher reads file contents from a provider's API. The API base URLs are
// overridable via GITHUB_API_BASE / BITBUCKET_API_BASE / CODECOMMIT_API_BASE
// (for testing against a local stub), matching internal/vcs/status.go and
// internal/githubapp.
type Fetcher struct {
	client        *http.Client
	githubBase    string
	bitbucketBase string
	cc            ccClients // CodeCommit API clients, cached per region/role
}

// NewFetcher builds a Fetcher from the environment. GITHUB_API_BASE and
// BITBUCKET_API_BASE default to the public API hosts.
func NewFetcher() *Fetcher {
	return &Fetcher{
		client:        &http.Client{Timeout: 15 * time.Second},
		githubBase:    apiBase("GITHUB_API_BASE", "https://api.github.com"),
		bitbucketBase: apiBase("BITBUCKET_API_BASE", "https://api.bitbucket.org"),
	}
}

// FetchFile returns the raw bytes of Path at SHA. found is false (with a nil
// error) when the file does not exist (HTTP 404, or CodeCommit's
// FileDoesNotExist family) or when the provider has no supported contents API
// ("other" or empty) — all of which mean "fall back". A non-404 HTTP failure or
// a transport error returns a non-nil error whose text never contains the token,
// and never a response body.
func (f *Fetcher) FetchFile(ctx context.Context, req FetchRequest) (content []byte, found bool, err error) {
	path := req.Path
	if path == "" {
		path = DefaultConfigPath
	}
	repo := RepoRef{
		Provider: req.Provider, Repo: req.Repo, CloneURL: req.CloneURL, Token: req.Token,
		Region: req.Region, Profile: req.Profile, RoleARN: req.RoleARN,
	}
	p := Lookup(req.Provider)
	if p.Name() == "git" && (!gitReachable(repo.CloneURL) || !gitAvailable()) {
		return nil, false, nil // nothing to read from: fall back to the registered config
	}
	return p.FetchFile(ctx, f, repo, req.SHA, path)
}

func decodeGitHubContents(body []byte) ([]byte, error) {
	var obj struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, fmt.Errorf("vcs: decode contents response: %w", err)
	}
	if obj.Encoding != "base64" {
		return nil, fmt.Errorf("vcs: unexpected contents encoding %q", obj.Encoding)
	}
	clean := strings.NewReplacer("\n", "", "\r", "").Replace(obj.Content)
	decoded, err := base64.StdEncoding.DecodeString(clean)
	if err != nil {
		return nil, fmt.Errorf("vcs: base64 decode contents: %w", err)
	}
	return decoded, nil
}

// pathEscape percent-escapes each segment of a repo-relative path while keeping
// the slashes, so nested paths like "ci/.forge-ci.yml" resolve correctly.
func pathEscape(p string) string {
	parts := strings.Split(p, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}
