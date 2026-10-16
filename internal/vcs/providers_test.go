package vcs

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestRegistryHasEveryHost(t *testing.T) {
	for _, n := range []string{"github", "gitlab", "bitbucket", "gitea", "codecommit", "git"} {
		if !Known(n) || Lookup(n).Name() != n {
			t.Errorf("provider %q not registered", n)
		}
	}
	if Lookup("other").Name() != "git" || Lookup("some-new-host").Name() != "git" {
		t.Error(`"other" and unknown hosts must resolve to the generic git provider`)
	}
}

func TestGitLabProvider(t *testing.T) {
	var gotPath, gotAuth string
	var gotStatus map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.EscapedPath()+"?"+r.URL.RawQuery, r.Header.Get("Authorization")
		switch {
		case strings.Contains(r.URL.EscapedPath(), "/repository/files/"):
			_, _ = w.Write([]byte("stages: [b]\n"))
		case strings.Contains(r.URL.Path, "/repository/compare"):
			_, _ = w.Write([]byte(`{"diffs":[{"old_path":"a.go","new_path":"a.go"},{"old_path":"old.md","new_path":"new.md"}]}`))
		case strings.Contains(r.URL.Path, "/statuses/"):
			_ = json.NewDecoder(r.Body).Decode(&gotStatus)
			w.WriteHeader(201)
		}
	}))
	defer srv.Close()
	t.Setenv("GITLAB_API_BASE", srv.URL)
	f := NewFetcher()
	repo := RepoRef{Provider: "gitlab", Repo: "acme/platform/cos", CloneURL: "https://gitlab.acme.io/acme/platform/cos.git", Token: "glpat"}

	body, found, err := f.FetchFile(context.Background(), FetchRequest{Provider: "gitlab", Repo: repo.Repo, CloneURL: repo.CloneURL, Token: "glpat", SHA: "abc", Path: "ci/forge.yml"})
	if err != nil || !found || string(body) != "stages: [b]\n" {
		t.Fatalf("fetch = %q %v %v", body, found, err)
	}
	if gotPath != "/projects/acme%2Fplatform%2Fcos/repository/files/ci%2Fforge.yml/raw?ref=abc" || gotAuth != "Bearer glpat" {
		t.Errorf("fetch request = %s (%s)", gotPath, gotAuth)
	}
	files, complete, err := f.ChangedFiles(context.Background(), CompareRequest{Provider: "gitlab", Repo: repo.Repo, CloneURL: repo.CloneURL, Base: "main", Head: "abc", Token: "glpat"})
	sort.Strings(files)
	if err != nil || !complete || strings.Join(files, ",") != "a.go,new.md,old.md" {
		t.Errorf("changes = %v %v %v", files, complete, err)
	}
	p := NewPoster()
	if err := p.Post(context.Background(), PostRequest{Provider: "gitlab", Repo: repo.Repo, CloneURL: repo.CloneURL, SHA: "abc", Token: "glpat", Status: "blocked", TargetURL: "https://forge/p/1"}); err != nil {
		t.Fatal(err)
	}
	if gotStatus["state"] != "pending" || gotStatus["name"] != StatusContext || gotStatus["target_url"] != "https://forge/p/1" {
		t.Errorf("status = %v", gotStatus)
	}
	if link, _ := Lookup("gitlab").FileURL(repo, "abc", ".forge-ci.yml"); link != "https://gitlab.acme.io/acme/platform/cos/-/blob/abc/.forge-ci.yml" {
		t.Errorf("link = %s", link)
	}
	if ui := Lookup("gitlab").CloneUser("tok"); ui.Username() != "oauth2" {
		t.Errorf("clone user = %v", ui)
	}
}

func TestGiteaProvider(t *testing.T) {
	var gotPath, gotAuth string
	var gotStatus map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path+"?"+r.URL.RawQuery, r.Header.Get("Authorization")
		switch {
		case strings.Contains(r.URL.Path, "/raw/"):
			_, _ = w.Write([]byte("jobs: {}\n"))
		case strings.Contains(r.URL.Path, "/compare/"):
			_, _ = w.Write([]byte(`{"files":[{"filename":"web/app.ts"}]}`))
		case strings.Contains(r.URL.Path, "/statuses/"):
			_ = json.NewDecoder(r.Body).Decode(&gotStatus)
			w.WriteHeader(201)
		}
	}))
	defer srv.Close()
	t.Setenv("GITEA_API_BASE", srv.URL)
	f := NewFetcher()
	body, found, err := f.FetchFile(context.Background(), FetchRequest{Provider: "gitea", Repo: "acme/ecom", CloneURL: "https://git.acme.io/acme/ecom.git", Token: "gt", SHA: "abc"})
	if err != nil || !found || string(body) != "jobs: {}\n" || gotPath != "/repos/acme/ecom/raw/.forge-ci.yml?ref=abc" || gotAuth != "token gt" {
		t.Fatalf("fetch = %q %v %v (%s %s)", body, found, err, gotPath, gotAuth)
	}
	files, complete, _ := f.ChangedFiles(context.Background(), CompareRequest{Provider: "gitea", Repo: "acme/ecom", CloneURL: "https://git.acme.io/acme/ecom.git", Base: "main", Head: "abc"})
	if !complete || len(files) != 1 || files[0] != "web/app.ts" {
		t.Errorf("changes = %v %v", files, complete)
	}
	if err := NewPoster().Post(context.Background(), PostRequest{Provider: "gitea", Repo: "acme/ecom", SHA: "abc", Token: "gt", Status: "failed"}); err != nil {
		t.Fatal(err)
	}
	if gotStatus["state"] != "failure" || gotStatus["context"] != StatusContext {
		t.Errorf("status = %v", gotStatus)
	}
}

// gitRun runs git in dir for the test fixture.
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestGenericGitProvider reads a file and diffs two commits of a real repo
// over file:// — the path every git host without an API integration takes.
func TestGenericGitProvider(t *testing.T) {
	if !gitAvailable() {
		t.Skip("git not installed")
	}
	src := t.TempDir()
	gitRun(t, src, "init", "-q", "-b", "main")
	gitRun(t, src, "config", "uploadpack.allowFilter", "true")
	gitRun(t, src, "config", "uploadpack.allowAnySHA1InWant", "true")
	write := func(name, content string) {
		_ = os.MkdirAll(filepath.Dir(filepath.Join(src, name)), 0o755)
		if err := os.WriteFile(filepath.Join(src, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".forge-ci.yml", "stages: [build]\n")
	write("api/main.go", "package main\n")
	gitRun(t, src, "add", ".")
	gitRun(t, src, "commit", "-q", "-m", "one")
	first := gitRun(t, src, "rev-parse", "HEAD")
	write("web/app.ts", "x\n")
	gitRun(t, src, "add", ".")
	gitRun(t, src, "commit", "-q", "-m", "two")
	second := gitRun(t, src, "rev-parse", "HEAD")

	f := NewFetcher()
	cloneURL := "file://" + src
	body, found, err := f.FetchFile(context.Background(), FetchRequest{Provider: "git", Repo: "local/repo", CloneURL: cloneURL, SHA: second})
	if err != nil || !found || string(body) != "stages: [build]\n" {
		t.Fatalf("git fetch = %q %v %v", body, found, err)
	}
	if _, found, err := f.FetchFile(context.Background(), FetchRequest{Provider: "git", Repo: "local/repo", CloneURL: cloneURL, SHA: second, Path: "missing.yml"}); err != nil || found {
		t.Errorf("missing file = %v %v, want not found", found, err)
	}
	files, complete, err := f.ChangedFiles(context.Background(), CompareRequest{Provider: "other", Repo: "local/repo", CloneURL: cloneURL, Base: first, Head: second})
	if err != nil || !complete || strings.Join(files, ",") != "web/app.ts" {
		t.Errorf("git diff = %v %v %v", files, complete, err)
	}
	// A host API that fails falls back to git for the diff.
	t.Setenv("GITEA_API_BASE", "http://127.0.0.1:1")
	files, complete, err = f.ChangedFiles(context.Background(), CompareRequest{Provider: "gitea", Repo: "local/repo", CloneURL: cloneURL, Base: first, Head: second})
	if err != nil || !complete || strings.Join(files, ",") != "web/app.ts" {
		t.Errorf("fallback diff = %v %v %v", files, complete, err)
	}
}
