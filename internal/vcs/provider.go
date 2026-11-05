package vcs

import (
	"context"
	"errors"
	"net/url"
	"sort"
	"strings"
	"sync"
)

// Provider is one git host. Forge talks to every host — GitHub, GitLab,
// Bitbucket, Gitea, AWS CodeCommit, or any plain git server — through this
// interface; each host is one implementation, registered by name. Adding a
// host means adding one file, not editing switch statements across the code.
type Provider interface {
	// Name is the registry key ("github", "gitlab", ..., "git").
	Name() string
	// FetchFile reads path at sha. found=false (nil error) when the file,
	// commit or repo isn't there, so callers fall back cleanly.
	FetchFile(ctx context.Context, f *Fetcher, repo RepoRef, sha, path string) (content []byte, found bool, err error)
	// ChangedFiles lists files changed between base and head (base may be a
	// branch; hosts that can diff from the merge base do). complete=false
	// when the list may be truncated.
	ChangedFiles(ctx context.Context, f *Fetcher, repo RepoRef, base, head string) (files []string, complete bool, err error)
	// StatusRequest builds the commit-status call for a pipeline phase, or
	// returns ErrNoStatusAPI when the host has none.
	StatusRequest(p *Poster, req PostRequest) (*StatusCall, error)
	// FileURL deep-links path at sha in the host's web UI (ok=false: none).
	FileURL(repo RepoRef, sha, path string) (link string, ok bool)
	// CloneUser is how a token is embedded in an HTTPS clone URL for this
	// host (nil when the host takes no token, e.g. signed CodeCommit URLs).
	CloneUser(token string) *url.Userinfo
}

// RepoRef is a repo as registered: where it lives and how to authenticate.
type RepoRef struct {
	Provider string
	Repo     string // owner/name, group/sub/project, or a CodeCommit repository name
	CloneURL string
	Token    string
	Region   string // CodeCommit
	Profile  string // CodeCommit
	RoleARN  string // CodeCommit
}

// StatusCall is one commit-status HTTP call.
type StatusCall struct {
	URL     string
	Body    []byte
	Headers map[string]string
}

// ErrNoStatusAPI: the host has no commit-status API (Forge may report
// another way, e.g. a CodeCommit pull-request comment, or not at all).
var ErrNoStatusAPI = errors.New("provider has no commit-status API")

var (
	regMu     sync.RWMutex
	providers = map[string]Provider{}
	aliases   = map[string]string{"other": "git"}
)

// Register adds a provider (called from each provider file's init).
func Register(p Provider) {
	regMu.Lock()
	defer regMu.Unlock()
	providers[p.Name()] = p
}

// Lookup returns the provider for a registered name; unknown names (and the
// legacy "other") resolve to the generic git provider, so any git server works.
func Lookup(name string) Provider {
	regMu.RLock()
	defer regMu.RUnlock()
	if a, ok := aliases[name]; ok {
		name = a
	}
	if p, ok := providers[name]; ok {
		return p
	}
	return providers["git"]
}

// Known reports whether name is a registered provider (or alias).
func Known(name string) bool {
	regMu.RLock()
	defer regMu.RUnlock()
	if _, ok := aliases[name]; ok {
		return true
	}
	_, ok := providers[name]
	return ok
}

// Names lists registered providers, sorted.
func Names() []string {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]string, 0, len(providers))
	for n := range providers {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// hostBase derives "https://<host>" from a clone URL (https or scp-style ssh),
// for self-hosted GitLab/Gitea API and web links.
func hostBase(cloneURL, fallback string) string {
	if h := webHost(cloneURL, ""); h != "" {
		scheme := "https"
		if u, err := url.Parse(cloneURL); err == nil && u.Scheme == "http" {
			scheme = "http"
		}
		return scheme + "://" + h
	}
	return fallback
}

// projectPath trims a leading/trailing slash and a .git suffix.
func projectPath(repo string) string {
	return strings.TrimSuffix(strings.Trim(strings.TrimSpace(repo), "/"), ".git")
}
