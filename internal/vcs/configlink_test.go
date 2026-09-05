package vcs

import (
	"strings"
	"testing"
)

func TestConfigFileURL(t *testing.T) {
	tests := []struct {
		name string
		req  ConfigLinkRequest
		want string
	}{
		{
			name: "github",
			req: ConfigLinkRequest{
				Provider: "github", Repo: "acme/checkout", SHA: "deadbeef",
				CloneURL: "https://github.com/acme/checkout.git",
			},
			want: "https://github.com/acme/checkout/blob/deadbeef/.forge-ci.yml",
		},
		{
			name: "bitbucket",
			req: ConfigLinkRequest{
				Provider: "bitbucket", Repo: "team/repo", SHA: "cafebabe",
				CloneURL: "https://bitbucket.org/team/repo.git",
			},
			want: "https://bitbucket.org/team/repo/src/cafebabe/.forge-ci.yml",
		},
		{
			name: "codecommit console browser",
			req: ConfigLinkRequest{
				Provider: "codecommit", Repo: "tablespace-api", SHA: "9fceb02d",
				CloneURL: "codecommit::ap-south-1://tablespace-api", Region: "ap-south-1",
			},
			want: "https://ap-south-1.console.aws.amazon.com/codesuite/codecommit/" +
				"repositories/tablespace-api/browse/9fceb02d/--/.forge-ci.yml?region=ap-south-1",
		},
		{
			name: "custom config path is honoured",
			req: ConfigLinkRequest{
				Provider: "github", Repo: "acme/checkout", SHA: "deadbeef",
				Path: "ci/pipeline.yml", CloneURL: "https://github.com/acme/checkout.git",
			},
			want: "https://github.com/acme/checkout/blob/deadbeef/ci/pipeline.yml",
		},
		{
			name: "leading slash on the path is normalised away",
			req: ConfigLinkRequest{
				Provider: "github", Repo: "acme/checkout", SHA: "deadbeef",
				Path: "/.forge-ci.yml",
			},
			want: "https://github.com/acme/checkout/blob/deadbeef/.forge-ci.yml",
		},
		{
			// A self-hosted install must link to its own host, taken from the
			// registered clone URL rather than assumed.
			name: "github enterprise host from the clone URL",
			req: ConfigLinkRequest{
				Provider: "github", Repo: "acme/checkout", SHA: "deadbeef",
				CloneURL: "https://git.internal.example.com/acme/checkout.git",
			},
			want: "https://git.internal.example.com/acme/checkout/blob/deadbeef/.forge-ci.yml",
		},
		{
			name: "clone URL port is preserved",
			req: ConfigLinkRequest{
				Provider: "bitbucket", Repo: "team/repo", SHA: "cafebabe",
				CloneURL: "https://bb.internal.example.com:7990/team/repo.git",
			},
			want: "https://bb.internal.example.com:7990/team/repo/src/cafebabe/.forge-ci.yml",
		},
		{
			name: "codecommit region falls back to the clone URL",
			req: ConfigLinkRequest{
				Provider: "codecommit", Repo: "widgets", SHA: "abc",
				CloneURL: "codecommit::eu-west-2://widgets",
			},
			want: "https://eu-west-2.console.aws.amazon.com/codesuite/codecommit/" +
				"repositories/widgets/browse/abc/--/.forge-ci.yml?region=eu-west-2",
		},
		{
			// No clone URL at all still yields a correct public link.
			name: "public host fallback",
			req: ConfigLinkRequest{
				Provider: "github", Repo: "acme/checkout", SHA: "deadbeef",
			},
			want: "https://github.com/acme/checkout/blob/deadbeef/.forge-ci.yml",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ConfigFileURL(tt.req)
			if !ok {
				t.Fatalf("ConfigFileURL(%+v) returned no link", tt.req)
			}
			if got != tt.want {
				t.Errorf("ConfigFileURL =\n  %q\nwant\n  %q", got, tt.want)
			}
		})
	}
}

// A page with no link is better than one with a link that 404s, so every case
// where a correct URL cannot be built must report no link rather than guess.
func TestConfigFileURLNoLink(t *testing.T) {
	tests := []struct {
		name string
		req  ConfigLinkRequest
	}{
		{
			name: `provider "other" has no known web UI`,
			req: ConfigLinkRequest{
				Provider: "other", Repo: "acme/thing", SHA: "abc",
				CloneURL: "https://git.example.com/acme/thing.git",
			},
		},
		{
			name: "unset provider",
			req:  ConfigLinkRequest{Repo: "acme/thing", SHA: "abc"},
		},
		{
			name: "unknown provider",
			req:  ConfigLinkRequest{Provider: "gitea", Repo: "acme/thing", SHA: "abc"},
		},
		{
			name: "missing sha",
			req:  ConfigLinkRequest{Provider: "github", Repo: "acme/thing"},
		},
		{
			name: "missing repo",
			req:  ConfigLinkRequest{Provider: "github", SHA: "abc"},
		},
		{
			// CodeCommit's file browser is in the region-scoped AWS console, so
			// without a region there is no URL to build.
			name: "codecommit with no region anywhere",
			req: ConfigLinkRequest{
				Provider: "codecommit", Repo: "widgets", SHA: "abc",
				CloneURL: "codecommit::://widgets",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ConfigFileURL(tt.req)
			if ok {
				t.Errorf("expected no link, got %q", got)
			}
			if got != "" {
				t.Errorf("expected an empty URL, got %q", got)
			}
		})
	}
}

// The link must pin the run's commit, never a branch name, or it would show
// whatever the file says now rather than what ran.
func TestConfigFileURLPinsTheRunCommit(t *testing.T) {
	const oldSHA = "1111111111111111111111111111111111111111"
	got, ok := ConfigFileURL(ConfigLinkRequest{
		Provider: "github", Repo: "acme/checkout", SHA: oldSHA,
		CloneURL: "https://github.com/acme/checkout.git",
	})
	if !ok {
		t.Fatal("no link")
	}
	if !strings.Contains(got, oldSHA) {
		t.Errorf("URL %q does not contain the run's sha %q", got, oldSHA)
	}
	for _, branchish := range []string{"/main/", "/master/", "HEAD"} {
		if strings.Contains(got, branchish) {
			t.Errorf("URL %q looks branch-pinned (%q), not commit-pinned", got, branchish)
		}
	}
}

// A nested config path keeps its separators while each segment is escaped, so
// `ci/forge/pipeline.yml` resolves rather than becoming one escaped blob.
func TestConfigFileURLEscapesPathSegmentsOnly(t *testing.T) {
	got, ok := ConfigFileURL(ConfigLinkRequest{
		Provider: "github", Repo: "acme/checkout", SHA: "abc",
		Path: "ci/forge ci/pipeline.yml",
	})
	if !ok {
		t.Fatal("no link")
	}
	if !strings.HasSuffix(got, "/ci/forge%20ci/pipeline.yml") {
		t.Errorf("URL = %q, want the path segments escaped but the slashes kept", got)
	}
}

func TestWebHost(t *testing.T) {
	tests := []struct{ cloneURL, fallback, want string }{
		{"https://github.com/a/b.git", "github.com", "github.com"},
		{"https://ghe.example.com/a/b.git", "github.com", "ghe.example.com"},
		{"https://ghe.example.com:8443/a/b.git", "github.com", "ghe.example.com:8443"},
		// Not an HTTPS URL: fall back rather than emit a broken host.
		{"git@github.com:a/b.git", "github.com", "github.com"},
		{"codecommit::ap-south-1://repo", "github.com", "github.com"},
		{"", "bitbucket.org", "bitbucket.org"},
		// An API host is not browsable.
		{"https://api.github.com/a/b", "github.com", "github.com"},
	}
	for _, tt := range tests {
		if got := webHost(tt.cloneURL, tt.fallback); got != tt.want {
			t.Errorf("webHost(%q, %q) = %q, want %q", tt.cloneURL, tt.fallback, got, tt.want)
		}
	}
}
