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
	"io"
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
	Provider string // github | bitbucket | other
	Repo     string // owner/name (github) or workspace/slug (bitbucket)
	SHA      string // the exact commit sha to read at
	Path     string // repo-relative file path (e.g. .forge-ci.yml)
	Token    string // plaintext auth; "" fetches anonymously (public repo)
}

// Fetcher reads file contents from a provider's API. The API base URLs are
// overridable via GITHUB_API_BASE / BITBUCKET_API_BASE (for testing against a
// local stub), matching internal/vcs/status.go and internal/githubapp.
type Fetcher struct {
	client        *http.Client
	githubBase    string
	bitbucketBase string
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
// error) when the file does not exist (HTTP 404) or when the provider has no
// supported contents API ("other" or empty) — both cases mean "fall back". A
// non-404 HTTP failure or a transport error returns a non-nil error whose text
// never contains the token.
func (f *Fetcher) FetchFile(ctx context.Context, req FetchRequest) (content []byte, found bool, err error) {
	owner, name, ok := splitRepo(req.Repo)
	if !ok {
		return nil, false, fmt.Errorf("invalid repo %q (want owner/name)", req.Repo)
	}
	path := req.Path
	if path == "" {
		path = DefaultConfigPath
	}

	var reqURL, accept string
	switch req.Provider {
	case "github":
		// contents API; raw media type returns the file bytes directly.
		reqURL = fmt.Sprintf("%s/repos/%s/%s/contents/%s?ref=%s",
			f.githubBase, owner, name, pathEscape(path), url.QueryEscape(req.SHA))
		accept = "application/vnd.github.raw"
	case "bitbucket":
		// src endpoint returns the raw file bytes.
		reqURL = fmt.Sprintf("%s/2.0/repositories/%s/%s/src/%s/%s",
			f.bitbucketBase, owner, name, url.PathEscape(req.SHA), pathEscape(path))
		accept = "*/*"
	default:
		// provider "other" (or unset) has no supported contents API — not found.
		return nil, false, nil
	}

	r, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, false, err
	}
	if req.Token != "" {
		r.Header.Set("Authorization", "Bearer "+req.Token)
	}
	r.Header.Set("Accept", accept)

	resp, err := f.client.Do(r)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // configs are small; cap at 1 MiB
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, false, nil // file absent → fall back
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		// Status only — the body can echo request material; never the token.
		return nil, false, fmt.Errorf("vcs: fetch %s failed: HTTP %d", path, resp.StatusCode)
	}

	// GitHub honors the raw media type and returns the file bytes. If a base or
	// proxy instead returns the JSON contents object (base64-encoded), decode it.
	if req.Provider == "github" && strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
		decoded, derr := decodeGitHubContents(body)
		if derr != nil {
			return nil, false, derr
		}
		return decoded, true, nil
	}
	return body, true, nil
}

// decodeGitHubContents extracts the file bytes from a GitHub contents JSON
// object (encoding: base64). GitHub wraps base64 content at 60 columns, so
// newlines are stripped before decoding.
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
