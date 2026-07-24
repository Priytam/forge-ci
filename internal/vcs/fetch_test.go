package vcs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestFetchGitHubRaw covers the GitHub contents API happy path with the raw
// media type: URL construction (owner/name/path + ?ref=sha), the Authorization
// header, the raw Accept header, and that the response bytes are returned as-is.
func TestFetchGitHubRaw(t *testing.T) {
	const wantYAML = "stages: [test]\njobs:\n  t: { stage: test, script: echo hi }\n"
	var gotPath, gotQuery, gotAuth, gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		gotAuth, gotAccept = r.Header.Get("Authorization"), r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/vnd.github.raw")
		_, _ = w.Write([]byte(wantYAML))
	}))
	defer srv.Close()

	f := &Fetcher{client: srv.Client(), githubBase: srv.URL}
	content, found, err := f.FetchFile(context.Background(), FetchRequest{
		Provider: "github", Repo: "acme/checkout", SHA: "deadbeef", Path: ".forge-ci.yml", Token: "ghp_secret",
	})
	if err != nil || !found {
		t.Fatalf("FetchFile: found=%v err=%v", found, err)
	}
	if string(content) != wantYAML {
		t.Errorf("content = %q, want %q", content, wantYAML)
	}
	if gotPath != "/repos/acme/checkout/contents/.forge-ci.yml" {
		t.Errorf("path = %q", gotPath)
	}
	if gotQuery != "ref=deadbeef" {
		t.Errorf("query = %q, want ref=deadbeef", gotQuery)
	}
	if gotAuth != "Bearer ghp_secret" {
		t.Errorf("auth = %q", gotAuth)
	}
	if gotAccept != "application/vnd.github.raw" {
		t.Errorf("accept = %q", gotAccept)
	}
}

// TestFetchGitHubBase64 covers the fallback where the API returns the JSON
// contents object (encoding: base64) instead of raw bytes.
func TestFetchGitHubBase64(t *testing.T) {
	const wantYAML = "stages: [build]\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// GitHub wraps base64 at 60 cols; emulate an embedded newline.
		enc := base64.StdEncoding.EncodeToString([]byte(wantYAML))
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"content": enc[:4] + "\n" + enc[4:], "encoding": "base64",
		})
	}))
	defer srv.Close()

	f := &Fetcher{client: srv.Client(), githubBase: srv.URL}
	content, found, err := f.FetchFile(context.Background(), FetchRequest{
		Provider: "github", Repo: "acme/checkout", SHA: "abc", Path: ".forge-ci.yml",
	})
	if err != nil || !found {
		t.Fatalf("FetchFile: found=%v err=%v", found, err)
	}
	if string(content) != wantYAML {
		t.Errorf("content = %q, want %q", content, wantYAML)
	}
}

// TestFetchGitHubDefaultPathAndNoToken covers the default path (empty -> the
// default config file) and that no Authorization header is sent when the token
// is empty (public-repo fetch).
func TestFetchGitHubDefaultPathAndNoToken(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/vnd.github.raw")
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	f := &Fetcher{client: srv.Client(), githubBase: srv.URL}
	_, found, err := f.FetchFile(context.Background(), FetchRequest{
		Provider: "github", Repo: "acme/checkout", SHA: "abc", // Path empty, Token empty
	})
	if err != nil || !found {
		t.Fatalf("FetchFile: found=%v err=%v", found, err)
	}
	if gotPath != "/repos/acme/checkout/contents/"+DefaultConfigPath {
		t.Errorf("path = %q, want default %q", gotPath, DefaultConfigPath)
	}
	if gotAuth != "" {
		t.Errorf("auth = %q, want none for empty token", gotAuth)
	}
}

// TestFetchBitbucket covers the Bitbucket src endpoint URL construction and raw
// body return.
func TestFetchBitbucket(t *testing.T) {
	const wantYAML = "stages: [deploy]\n"
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		_, _ = w.Write([]byte(wantYAML))
	}))
	defer srv.Close()

	f := &Fetcher{client: srv.Client(), bitbucketBase: srv.URL}
	content, found, err := f.FetchFile(context.Background(), FetchRequest{
		Provider: "bitbucket", Repo: "team/service", SHA: "cafe123", Path: ".forge-ci.yml", Token: "bb_token",
	})
	if err != nil || !found {
		t.Fatalf("FetchFile: found=%v err=%v", found, err)
	}
	if string(content) != wantYAML {
		t.Errorf("content = %q", content)
	}
	if gotPath != "/2.0/repositories/team/service/src/cafe123/.forge-ci.yml" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "Bearer bb_token" {
		t.Errorf("auth = %q", gotAuth)
	}
}

// TestFetch404NotFound covers that a 404 is reported as (found=false, nil error)
// for both providers so callers fall back to the registered config.
func TestFetch404NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	for _, provider := range []string{"github", "bitbucket"} {
		f := &Fetcher{client: srv.Client(), githubBase: srv.URL, bitbucketBase: srv.URL}
		content, found, err := f.FetchFile(context.Background(), FetchRequest{
			Provider: provider, Repo: "a/b", SHA: "x", Path: ".forge-ci.yml", Token: "t",
		})
		if err != nil {
			t.Errorf("%s: err = %v, want nil on 404", provider, err)
		}
		if found {
			t.Errorf("%s: found = true, want false on 404", provider)
		}
		if content != nil {
			t.Errorf("%s: content = %q, want nil on 404", provider, content)
		}
	}
}

// TestFetchOtherProviderNotSupported covers that provider "other" (and unset) is
// treated as not-found without any HTTP call.
func TestFetchOtherProviderNotSupported(t *testing.T) {
	f := &Fetcher{client: http.DefaultClient} // no server; a call would fail
	for _, provider := range []string{"other", ""} {
		_, found, err := f.FetchFile(context.Background(), FetchRequest{
			Provider: provider, Repo: "a/b", SHA: "x", Path: ".forge-ci.yml",
		})
		if err != nil || found {
			t.Errorf("provider %q: found=%v err=%v, want found=false err=nil", provider, found, err)
		}
	}
}

// TestFetchServerErrorIsError covers that a non-404 failure (e.g. 500) returns a
// real error and never leaks the token in the message.
func TestFetchServerErrorIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	f := &Fetcher{client: srv.Client(), githubBase: srv.URL}
	_, found, err := f.FetchFile(context.Background(), FetchRequest{
		Provider: "github", Repo: "a/b", SHA: "x", Path: ".forge-ci.yml", Token: "ghp_supersecret",
	})
	if err == nil {
		t.Fatal("want error on HTTP 500")
	}
	if found {
		t.Error("found = true, want false on 500")
	}
	if got := err.Error(); strings.Contains(got, "ghp_supersecret") {
		t.Errorf("error text leaked token: %q", got)
	}
}

// TestFetchInvalidRepo covers rejection of a repo that is not owner/name.
func TestFetchInvalidRepo(t *testing.T) {
	f := NewFetcher()
	_, _, err := f.FetchFile(context.Background(), FetchRequest{Provider: "github", Repo: "noslash", SHA: "x"})
	if err == nil {
		t.Fatal("want error for repo without a slash")
	}
}
