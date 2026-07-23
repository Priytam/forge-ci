// forge-runner: pull-based build agent. Long-polls the control plane for
// pending jobs, executes them via the configured executor, streams logs back,
// and heartbeats so the server can detect dead runners.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	osexec "os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/executor"
	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

const (
	logFlushEvery     = 700 * time.Millisecond
	heartbeatEvery    = 10 * time.Second
	acquireRetryPause = 2 * time.Second
)

type client struct {
	base  string
	http  *http.Client
	token string // runner bearer token (empty when RUNNER_AUTH=off)
}

// do sends req, attaching the runner bearer token when one is configured.
func (c *client) do(req *http.Request) (*http.Response, error) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	return c.http.Do(req)
}

func (c *client) post(ctx context.Context, path string, body []byte, contentType string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	return c.do(req)
}

func (c *client) postJSON(ctx context.Context, path string, v any) (*http.Response, error) {
	b, _ := json.Marshal(v)
	return c.post(ctx, path, b, "application/json")
}

func main() {
	host, _ := os.Hostname()
	var (
		server   = flag.String("server", envOr("SERVER_URL", "http://localhost:8080"), "control plane base URL")
		execKind = flag.String("executor", envOr("EXECUTOR", "shell"), "executor: shell | docker")
		runnerID = flag.String("id", envOr("RUNNER_ID", fmt.Sprintf("%s-%d", host, os.Getpid())), "runner id")
		tagsFlag = flag.String("tags", envOr("RUNNER_TAGS", ""), "comma-separated runner tags (jobs route by tag)")
		token    = flag.String("token", envOr("RUNNER_TOKEN", ""), "runner auth token (required when the server runs RUNNER_AUTH=on)")
		conc     = flag.Int("concurrency", concFromEnv(), "jobs to run in parallel (each forks its own executor)")
	)
	flag.Parse()
	if *conc < 1 {
		*conc = 1
	}

	var tags []string
	for _, t := range strings.Split(*tagsFlag, ",") {
		if t = strings.TrimSpace(t); t != "" {
			tags = append(tags, t)
		}
	}

	exec, err := executor.New(*execKind)
	if err != nil {
		slog.Error("bad executor", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	c := &client{base: *server, http: &http.Client{Timeout: 60 * time.Second}, token: *token}
	slog.Info("forge-runner started", "id", *runnerID, "server", *server,
		"executor", exec.Name(), "tags", tags, "concurrency", *conc)

	// The manager stays resident; each slot forks work per acquired job
	// (with the kubernetes executor, that fork is an ephemeral pod).
	var wg sync.WaitGroup
	for slot := 0; slot < *conc; slot++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := proto.AcquireRequest{RunnerID: *runnerID, Executor: exec.Name(), Tags: tags}
			for ctx.Err() == nil {
				job, err := acquire(ctx, c, req)
				if err != nil {
					if ctx.Err() == nil {
						slog.Warn("acquire failed, retrying", "err", err)
						sleep(ctx, acquireRetryPause)
					}
					continue
				}
				if job == nil {
					continue // long-poll timed out, poll again
				}
				runJob(ctx, c, exec, job)
			}
		}()
	}
	wg.Wait()
}

func concFromEnv() int {
	if v := os.Getenv("RUNNER_CONCURRENCY"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
			return n
		}
	}
	return 1
}

func acquire(ctx context.Context, c *client, req proto.AcquireRequest) (*proto.RunnerJob, error) {
	resp, err := c.postJSON(ctx, "/api/v1/runner/acquire", req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent:
		return nil, nil
	case http.StatusOK:
		var job proto.RunnerJob
		if err := json.NewDecoder(resp.Body).Decode(&job); err != nil {
			return nil, err
		}
		return &job, nil
	default:
		return nil, fmt.Errorf("acquire: unexpected status %d", resp.StatusCode)
	}
}

func runJob(ctx context.Context, c *client, exec executor.Executor, job *proto.RunnerJob) {
	slog.Info("job started", "job", job.ID, "name", job.Name)
	logs := newLogStreamer(c, job.ID, job.RedactValues)
	logs.printf("Running job #%d %q on %s executor\n", job.ID, job.Name, exec.Name())

	hbCtx, stopHB := context.WithCancel(ctx)
	var hbWG sync.WaitGroup
	hbWG.Add(1)
	go func() {
		defer hbWG.Done()
		t := time.NewTicker(heartbeatEvery)
		defer t.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-t.C:
				resp, err := c.postJSON(hbCtx, fmt.Sprintf("/api/v1/runner/jobs/%d/heartbeat", job.ID), nil)
				if err == nil {
					resp.Body.Close()
				}
			}
		}
	}()

	finish := func(status string, exitCode int) {
		stopHB()
		hbWG.Wait()
		logs.close()
		// Fresh context: ctx may be canceled on shutdown.
		rctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		resp, err := c.postJSON(rctx, fmt.Sprintf("/api/v1/runner/jobs/%d/complete", job.ID),
			proto.CompleteRequest{Status: status, ExitCode: exitCode})
		if err != nil {
			slog.Error("failed to report completion", "job", job.ID, "err", err)
			return
		}
		resp.Body.Close()
		slog.Info("job finished", "job", job.ID, "status", status, "exit_code", exitCode)
	}

	status, exitCode := "success", 0
	workdir, err := os.MkdirTemp("", "forge-job-*")
	if err != nil {
		workdir = "."
	} else {
		defer os.RemoveAll(workdir)
	}

	if job.CloneURL != "" {
		if err := cloneSource(ctx, logs, job, workdir); err != nil {
			logs.printf("checkout failed: %v\n", err)
			finish("failed", 1)
			return
		}
	}

	// GitLab-style artifact passing: restore upstream jobs' artifacts into
	// the workspace before the script runs.
	for _, dep := range job.Dependencies {
		if err := restoreDependency(ctx, c, logs, dep, workdir); err != nil {
			logs.printf("restoring artifacts of %q failed: %v\n", dep.JobName, err)
			finish("failed", 1)
			return
		}
	}

	// Execution timeout: the context deadline kills the process (or the
	// kubectl exec driving the pod) when it elapses.
	timeout := time.Duration(job.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = time.Hour
	}
	jobCtx, cancelJob := context.WithTimeout(ctx, timeout)
	defer cancelJob()

	logs.printf("$ %s\n", job.Script)
	out, wait, err := exec.Start(jobCtx, job, workdir)
	if err != nil {
		status, exitCode = "failed", 1
		logs.printf("executor error: %v\n", err)
	} else {
		buf := make([]byte, 32*1024)
		for {
			n, rerr := out.Read(buf)
			if n > 0 {
				logs.write(buf[:n])
			}
			if rerr != nil {
				break
			}
		}
		exitCode = wait()
		if exitCode != 0 {
			status = "failed"
		}
		if jobCtx.Err() == context.DeadlineExceeded {
			status, exitCode = "failed", 124
			logs.printf("\nERROR: job timed out after %s and was killed\n", timeout)
		} else {
			logs.printf("\nJob exited with code %d\n", exitCode)
		}
	}

	if status == "success" && len(job.ArtifactPaths) > 0 {
		uploadArtifacts(c, logs, job, workdir)
	}
	finish(status, exitCode)
}

// cloneSource checks out the pipeline's SHA into the workspace via a shallow
// fetch. The clone URL (which may embed a token) is passed to git but never
// printed; the streamer additionally redacts RedactValues.
func cloneSource(ctx context.Context, logs *logStreamer, job *proto.RunnerJob, workdir string) error {
	short := job.SHA
	if len(short) > 8 {
		short = short[:8]
	}
	logs.printf("Checking out %s @ %s (%s)\n", job.RepoName, short, job.Ref)
	cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	run := func(args ...string) error {
		cmd := osexec.CommandContext(cctx, "git", args...)
		cmd.Dir = workdir
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		out, err := cmd.CombinedOutput()
		if err != nil {
			msg := string(out)
			for _, v := range job.RedactValues {
				msg = strings.ReplaceAll(msg, v, "[REDACTED]")
			}
			return fmt.Errorf("git %s: %s", args[0], strings.TrimSpace(msg))
		}
		return nil
	}
	if err := run("init", "-q"); err != nil {
		return err
	}
	if err := run("remote", "add", "origin", job.CloneURL); err != nil {
		return err
	}
	// Prefer the exact SHA; fall back to the ref tip (some servers refuse
	// direct SHA fetches).
	if err := run("fetch", "-q", "--depth", "1", "origin", job.SHA); err != nil {
		logs.printf("direct SHA fetch unavailable, fetching ref %s\n", job.Ref)
		if err := run("fetch", "-q", "--depth", "1", "origin", job.Ref); err != nil {
			return err
		}
	}
	if err := run("checkout", "-q", "FETCH_HEAD"); err != nil {
		return err
	}
	logs.printf("Checkout complete\n")
	return nil
}

// restoreDependency downloads an upstream job's artifact archive and unpacks
// it into the workspace.
func restoreDependency(ctx context.Context, c *client, logs *logStreamer, dep proto.DependencyArtifact, workdir string) error {
	rctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet,
		fmt.Sprintf("%s/api/v1/artifacts/%d/download", c.base, dep.ArtifactID), nil)
	if err != nil {
		return err
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned status %d", resp.StatusCode)
	}
	cmd := osexec.CommandContext(rctx, "tar", "-xzf", "-", "-C", workdir)
	cmd.Stdin = resp.Body
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("untar: %s", strings.TrimSpace(string(out)))
	}
	logs.printf("Restored artifacts of job %q (%s)\n", dep.JobName, dep.Name)
	return nil
}

// uploadArtifacts archives the declared workspace paths with tar and streams
// the archive to the control plane. Missing paths are skipped with a log line.
func uploadArtifacts(c *client, logs *logStreamer, job *proto.RunnerJob, workdir string) {
	var existing []string
	for _, p := range job.ArtifactPaths {
		clean := strings.TrimSuffix(strings.TrimSpace(p), "/")
		if clean == "" || strings.HasPrefix(clean, "/") || strings.HasPrefix(clean, "..") {
			logs.printf("artifacts: skipping unsafe path %q\n", p)
			continue
		}
		if _, err := os.Stat(workdir + "/" + clean); err != nil {
			logs.printf("artifacts: path %q not found in workspace, skipping\n", clean)
			continue
		}
		existing = append(existing, clean)
	}
	if len(existing) == 0 {
		logs.printf("artifacts: nothing to upload\n")
		return
	}

	archive, err := os.CreateTemp("", "forge-artifacts-*.tar.gz")
	if err != nil {
		logs.printf("artifacts: temp file error: %v\n", err)
		return
	}
	archive.Close()
	defer os.Remove(archive.Name())

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	tarArgs := append([]string{"-czf", archive.Name(), "-C", workdir}, existing...)
	if out, err := osexec.CommandContext(ctx, "tar", tarArgs...).CombinedOutput(); err != nil {
		logs.printf("artifacts: tar failed: %v: %s\n", err, out)
		return
	}
	f, err := os.Open(archive.Name())
	if err != nil {
		logs.printf("artifacts: open archive: %v\n", err)
		return
	}
	defer f.Close()
	stat, _ := f.Stat()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/api/v1/runner/jobs/%d/artifacts?name=artifacts.tar.gz", c.base, job.ID), f)
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/gzip")
	resp, err := c.do(req)
	if err != nil {
		logs.printf("artifacts: upload failed: %v\n", err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		logs.printf("artifacts: upload rejected with status %d\n", resp.StatusCode)
		return
	}
	logs.printf("artifacts: uploaded %d bytes (%s)\n", stat.Size(), strings.Join(existing, ", "))
}

// logStreamer batches log bytes and flushes them to the server periodically.
type logStreamer struct {
	c      *client
	jobID  int64
	redact []string
	mu     sync.Mutex
	buf    bytes.Buffer
	done   chan struct{}
	wg     sync.WaitGroup
}

func newLogStreamer(c *client, jobID int64, redact []string) *logStreamer {
	ls := &logStreamer{c: c, jobID: jobID, redact: redact, done: make(chan struct{})}
	ls.wg.Add(1)
	go func() {
		defer ls.wg.Done()
		t := time.NewTicker(logFlushEvery)
		defer t.Stop()
		for {
			select {
			case <-ls.done:
				return
			case <-t.C:
				ls.flush()
			}
		}
	}()
	return ls
}

func (ls *logStreamer) write(p []byte) {
	ls.mu.Lock()
	ls.buf.Write(p)
	ls.mu.Unlock()
}

func (ls *logStreamer) printf(format string, args ...any) {
	ls.write(fmt.Appendf(nil, format, args...))
}

func (ls *logStreamer) flush() {
	ls.mu.Lock()
	if ls.buf.Len() == 0 {
		ls.mu.Unlock()
		return
	}
	chunk := ls.buf.String()
	ls.buf.Reset()
	ls.mu.Unlock()
	for _, v := range ls.redact {
		chunk = strings.ReplaceAll(chunk, v, "[REDACTED]")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := ls.c.post(ctx, fmt.Sprintf("/api/v1/runner/jobs/%d/logs", ls.jobID),
		[]byte(chunk), "text/plain")
	if err != nil {
		slog.Warn("log flush failed", "job", ls.jobID, "err", err)
		return
	}
	resp.Body.Close()
}

func (ls *logStreamer) close() {
	close(ls.done)
	ls.wg.Wait()
	ls.flush()
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
