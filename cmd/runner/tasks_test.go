package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/mirror"
	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

// TestTaskLoop drives the runner's task loop against a stub server: it takes
// a diff task, computes it from its mirror and reports the result; a failing
// task reports an error with the credential redacted.
func TestTaskLoop(t *testing.T) {
	if !mirror.Supported() {
		t.Skip("git 2.27+ not installed")
	}
	dir := t.TempDir()
	git := func(d string, args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = d
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t",
			"GIT_COMMITTER_EMAIL=t@x", "GIT_CONFIG_GLOBAL=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	bare, work := filepath.Join(dir, "o.git"), filepath.Join(dir, "w")
	git(dir, "init", "-q", "--bare", "-b", "main", bare)
	git(bare, "config", "uploadpack.allowFilter", "true")
	git(dir, "clone", "-q", bare, work)
	_ = os.WriteFile(filepath.Join(work, "a.txt"), []byte("1\n"), 0o644)
	git(work, "add", "-A")
	git(work, "commit", "-q", "-m", "one")
	from := git(work, "rev-parse", "HEAD")
	_ = os.WriteFile(filepath.Join(work, "a.txt"), []byte("1\n2\n"), 0o644)
	git(work, "commit", "-q", "-am", "two")
	to := git(work, "rev-parse", "HEAD")
	git(work, "push", "-q", "origin", "main")

	t.Setenv("RUNNER_MIRROR_DIR", filepath.Join(dir, "mirrors"))
	tasks := []proto.RunnerTask{
		{ID: 1, Kind: proto.TaskKindDiff, Repo: "acme/x", CloneURL: "file://" + bare,
			Diff: &proto.DiffTask{From: from, To: to, Mode: "range", FetchRefs: []string{"+refs/heads/main:refs/heads/main"}}},
		{ID: 2, Kind: proto.TaskKindDiff, Repo: "acme/y", CloneURL: "file://" + dir + "/nope-s3cret.git", RedactValues: []string{"s3cret"},
			Diff: &proto.DiffTask{From: from, To: to, Mode: "range", FetchRefs: []string{"+refs/heads/main:refs/heads/main"}}},
	}
	var mu sync.Mutex
	reports := map[string]proto.TaskCompleteRequest{}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/api/v1/runner/tasks/acquire":
			if len(tasks) == 0 {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			_ = json.NewEncoder(w).Encode(tasks[0])
			tasks = tasks[1:]
		case strings.HasSuffix(r.URL.Path, "/complete"):
			var rep proto.TaskCompleteRequest
			_ = json.NewDecoder(r.Body).Decode(&rep)
			reports[r.URL.Path] = rep
			if len(reports) == 2 {
				cancel()
			}
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()
	taskLoop(ctx, &client{base: srv.URL, http: srv.Client()}, "runner-t")

	ok := reports["/api/v1/runner/tasks/1/complete"]
	if ok.Error != "" || ok.Diff == nil || len(ok.Diff.Files) != 1 || ok.Diff.Files[0].Additions != 1 || ok.RunnerID != "runner-t" {
		t.Errorf("diff report = %+v", ok)
	}
	bad := reports["/api/v1/runner/tasks/2/complete"]
	if bad.Error == "" || strings.Contains(bad.Error, "s3cret") {
		t.Errorf("failure report = %q (must exist and never carry the credential)", bad.Error)
	}
}

// TestTaskLoopBacksOffOnFastEmpty: a server that answers "no task" at once
// must not turn the loop into a hot loop.
func TestTaskLoopBacksOffOnFastEmpty(t *testing.T) {
	if !mirror.Supported() {
		t.Skip("git 2.27+ not found")
	}
	t.Setenv("RUNNER_MIRROR_DIR", t.TempDir())
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	taskLoop(ctx, &client{base: srv.URL, http: srv.Client()}, "runner-t")
	if n := calls.Load(); n > 1 {
		t.Fatalf("%d acquires in 1s against an instant 204; want 1 (then a %v pause)", n, acquireRetryPause)
	}
}
