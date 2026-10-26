package vcstest

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/vcs"
)

// FakeGit is a plain git server: a bare repository on disk reached over
// file://, with a work tree to commit from. It has no API, no webhooks and no
// rate limits — exactly what a self-hosted git server offers.
type FakeGit struct {
	bare, work string
	next       int
}

// NewFakeGit creates the bare repo with one commit on main. It skips the test
// when git is not installed.
func NewFakeGit(t *testing.T) *FakeGit {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	g := &FakeGit{bare: filepath.Join(dir, "origin.git"), work: filepath.Join(dir, "work"), next: 1}
	g.git(t, dir, "init", "-q", "--bare", "-b", "main", g.bare)
	g.git(t, dir, "clone", "-q", g.bare, g.work)
	g.commit(t, "main", "README.md")
	return g
}

func (g *FakeGit) git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com", "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commit adds a commit on branch (creating it from main) and pushes it.
func (g *FakeGit) commit(t *testing.T, branch, file string) string {
	t.Helper()
	if branch != "main" {
		if _, err := os.Stat(filepath.Join(g.work, ".git", "refs", "heads", branch)); err != nil {
			g.git(t, g.work, "checkout", "-q", "-B", branch, "origin/main")
		} else {
			g.git(t, g.work, "checkout", "-q", branch)
		}
	}
	path := filepath.Join(g.work, file)
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(time.Now().String()), 0o644); err != nil {
		t.Fatal(err)
	}
	g.git(t, g.work, "add", "-A")
	g.git(t, g.work, "commit", "-q", "-m", "change "+file)
	g.git(t, g.work, "push", "-q", "origin", branch)
	return g.git(t, g.work, "rev-parse", "HEAD")
}

func (g *FakeGit) Repo() vcs.RepoRef {
	return vcs.RepoRef{Provider: "git", Repo: "acme/plain", CloneURL: "file://" + g.bare}
}

func (g *FakeGit) Open(t *testing.T, title string) (string, *vcs.Change) {
	id := strconv.Itoa(g.next)
	g.next++
	branch := "change/" + id
	g.commit(t, branch, "f"+id+".txt")
	return id, &vcs.Change{ID: id, Title: title, SourceBranch: branch, TargetBranch: "main", State: "open"}
}

func (g *FakeGit) Push(t *testing.T, id string) string {
	return g.commit(t, "change/"+id, "f"+id+"-"+strconv.FormatInt(time.Now().UnixNano(), 10)+".txt")
}

func (g *FakeGit) Close(t *testing.T, id string) {
	g.git(t, g.work, "push", "-q", "origin", "--delete", "change/"+id)
}

func (g *FakeGit) Review(*testing.T, string, string, string) {}

func (g *FakeGit) Delivery(*testing.T, string, string) (http.Header, []byte, bool) {
	return nil, nil, false
}

func (g *FakeGit) Unrelated(*testing.T) (http.Header, []byte, bool) {
	return http.Header{}, []byte(`{"ref":"refs/heads/main"}`), true
}

func (g *FakeGit) RateLimit(int, int, time.Time) {}
func (g *FakeGit) RejectNext(int, time.Duration) {}

func (g *FakeGit) Head(t *testing.T, id string) string {
	out := g.git(t, g.work, "ls-remote", g.bare, "refs/heads/change/"+id)
	sha, _, _ := strings.Cut(out, "\t")
	return sha
}

// A plain git server has no status checks and no merge API.
func (g *FakeGit) Protect(...string)                       {}
func (g *FakeGit) Check(*testing.T, string, string) string { return "" }
func (g *FakeGit) HostMerge(*testing.T, string) error      { return nil }

// A plain git server has no comments: threads on it live in Forge.
func (g *FakeGit) HostThread(*testing.T, string, string, string) string { return "" }
func (g *FakeGit) CommentsOn(*testing.T, string) []string               { return nil }
