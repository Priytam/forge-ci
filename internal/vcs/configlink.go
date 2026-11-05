package vcs

// Deep links to the pipeline config a run actually used.
//
// Forge does not host the repo, so the honest way to show "the .forge-ci.yml
// this pipeline ran" is to point at the file in the provider's own web UI AT
// THE RUN'S COMMIT — not at the branch tip, which may have moved on since.
//
// The web host is taken from the registered clone URL when it is an HTTPS URL,
// so a self-hosted GitHub Enterprise or Bitbucket Server install links to its
// own host rather than the public one. Only the public fallbacks are hardcoded.

import (
	"net/url"
	"strings"
)

// ConfigLinkRequest is one config-file deep link to build.
type ConfigLinkRequest struct {
	Provider string // github | bitbucket | codecommit (anything else has no link)
	Repo     string // owner/name, or the repository name for codecommit
	SHA      string // the pipeline's commit — the file as it was for THIS run
	Path     string // repo-relative config path ("" = .forge-ci.yml)
	CloneURL string // registered clone URL; supplies the host and codecommit region
	Region   string // codecommit region (falls back to the clone URL's)
}

// ConfigFileURL builds a browser URL for a repo's config file at an exact
// commit. ok is false whenever a correct link cannot be produced — an unknown
// or "other" provider, a missing sha, a missing region for CodeCommit — because
// a page with no link is better than one with a link that 404s.
func ConfigFileURL(req ConfigLinkRequest) (string, bool) {
	sha := strings.TrimSpace(req.SHA)
	repo := strings.Trim(strings.TrimSpace(req.Repo), "/")
	if sha == "" || repo == "" {
		return "", false
	}
	path := strings.TrimSpace(req.Path)
	if path == "" {
		path = DefaultConfigPath
	}
	path = strings.TrimPrefix(path, "/")

	return Lookup(req.Provider).FileURL(RepoRef{
		Provider: req.Provider, Repo: repo, CloneURL: req.CloneURL, Region: req.Region,
	}, sha, path)
}

// webHost returns the host to build a web URL against: the registered clone
// URL's host when it is an HTTPS URL (so self-hosted installs link to
// themselves), else the provider's public host.
func webHost(cloneURL, fallback string) string {
	u, err := url.Parse(strings.TrimSpace(cloneURL))
	if err != nil || !strings.HasPrefix(u.Scheme, "http") || u.Hostname() == "" {
		return fallback
	}
	host := u.Hostname()
	// api.github.com is an API host, not a browsable one; a clone URL should
	// never be that, but never emit a link into it if it is.
	if strings.HasPrefix(host, "api.") {
		return fallback
	}
	if port := u.Port(); port != "" {
		host += ":" + port
	}
	return host
}
