// forge-runner: pull-based build agent. Long-polls the control plane for
// pending jobs, executes them via the configured executor, streams logs back,
// and heartbeats so the server can detect dead runners.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	osexec "os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/executor"
	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

const (
	logFlushEvery     = 700 * time.Millisecond
	heartbeatEvery    = 10 * time.Second
	acquireRetryPause = 2 * time.Second
	defaultDrainGrace = 30 * time.Second
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

	// drainCtx is canceled on SIGINT/SIGTERM and stops acquisition of NEW jobs.
	// execCtx stays alive through a grace period so in-flight jobs can finish;
	// after the grace elapses it is canceled to kill (and requeue) the rest.
	drainCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	execCtx, cancelExec := context.WithCancel(context.Background())
	defer cancelExec()

	grace := drainGraceFromEnv()
	jobsDone := make(chan struct{})
	go func() {
		select {
		case <-drainCtx.Done():
		case <-jobsDone:
			return
		}
		slog.Info("drain: signal received; not acquiring new jobs; finishing in-flight", "grace", grace)
		select {
		case <-time.After(grace):
			slog.Warn("drain: grace elapsed; requeuing remaining in-flight jobs")
			cancelExec()
		case <-jobsDone:
		}
	}()

	c := &client{base: *server, http: &http.Client{Timeout: 60 * time.Second}, token: *token}
	slog.Info("forge-runner started", "id", *runnerID, "server", *server,
		"executor", exec.Name(), "tags", tags, "concurrency", *conc, "drain_grace", grace)

	// The manager stays resident; each slot forks work per acquired job
	// (with the kubernetes executor, that fork is an ephemeral pod).
	var wg sync.WaitGroup
	for slot := 0; slot < *conc; slot++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := proto.AcquireRequest{RunnerID: *runnerID, Executor: exec.Name(), Tags: tags}
			for drainCtx.Err() == nil {
				job, err := acquire(drainCtx, c, req)
				if err != nil {
					if drainCtx.Err() == nil {
						slog.Warn("acquire failed, retrying", "err", err)
						sleep(drainCtx, acquireRetryPause)
					}
					continue
				}
				if job == nil {
					continue // long-poll timed out, poll again
				}
				runJob(execCtx, drainCtx, c, exec, job)
			}
		}()
	}
	wg.Wait()
	close(jobsDone)
}

func drainGraceFromEnv() time.Duration {
	if v := os.Getenv("RUNNER_DRAIN_GRACE"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 0 {
			return d
		}
	}
	return defaultDrainGrace
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

// runJob executes one job. execCtx is the execution context (canceled on drain
// only after the grace period); drainCtx signals that the runner is shutting
// down. A job killed because cancel was requested via heartbeat reports
// 'canceled'; a job killed by drain reports 'requeue' so another runner repicks
// it; a timeout reports 'failed' exit 124.
func runJob(execCtx, drainCtx context.Context, c *client, exec executor.Executor, job *proto.RunnerJob) {
	slog.Info("job started", "job", job.ID, "name", job.Name)
	logs := newLogStreamer(c, job.ID, job.RedactValues)
	logs.printf("Running job #%d %q on %s executor\n", job.ID, job.Name, exec.Name())

	// Execution timeout: the context deadline kills the process (or the
	// kubectl exec driving the pod) when it elapses. Derived from execCtx so a
	// post-grace drain also kills it.
	timeout := time.Duration(job.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = time.Hour
	}
	jobCtx, cancelJob := context.WithTimeout(execCtx, timeout)
	defer cancelJob()

	// canceledByRequest is set when the server signals cancellation through a
	// heartbeat response; it distinguishes a requested cancel (report
	// 'canceled') from a drain kill (report 'requeue') or a timeout.
	var canceledByRequest atomic.Bool

	hbCtx, stopHB := context.WithCancel(context.Background())
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
				if err != nil {
					continue
				}
				var hb proto.HeartbeatResponse
				_ = json.NewDecoder(resp.Body).Decode(&hb)
				resp.Body.Close()
				if hb.Cancel && canceledByRequest.CompareAndSwap(false, true) {
					logs.printf("\nJob canceled by request; stopping\n")
					slog.Info("job cancel signaled by server", "job", job.ID)
					cancelJob()
				}
			}
		}
	}()

	finish := func(status string, exitCode int) {
		stopHB()
		hbWG.Wait()
		logs.close()
		// Fresh context: execCtx/drainCtx may be canceled on shutdown.
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

	// resolveInterruption maps a killed/failed execution to the right terminal
	// status: requested-cancel > timeout > drain-requeue > plain failure.
	resolveInterruption := func(status string, exitCode int) (string, int) {
		switch {
		case canceledByRequest.Load():
			return "canceled", exitCode
		case jobCtx.Err() == context.DeadlineExceeded:
			return "failed", 124
		case execCtx.Err() != nil:
			return "requeue", exitCode
		default:
			return status, exitCode
		}
	}

	status, exitCode := "success", 0
	workdir, err := os.MkdirTemp("", "forge-job-*")
	if err != nil {
		workdir = "."
	} else {
		defer os.RemoveAll(workdir)
	}

	if job.CloneURL != "" {
		if err := cloneSource(jobCtx, logs, job, workdir); err != nil {
			logs.printf("checkout failed: %v\n", err)
			st, code := resolveInterruption("failed", 1)
			finish(st, code)
			return
		}
	}

	// GitLab-style artifact passing: restore upstream jobs' artifacts into
	// the workspace before the script runs.
	for _, dep := range job.Dependencies {
		if err := restoreDependency(jobCtx, c, logs, dep, workdir); err != nil {
			logs.printf("restoring artifacts of %q failed: %v\n", dep.JobName, err)
			st, code := resolveInterruption("failed", 1)
			finish(st, code)
			return
		}
	}

	// Cache restore (GitLab-style): before the script, when the policy allows a
	// pull. A miss or any failure is non-fatal — the job runs regardless.
	primaryCacheKey, restoreCacheKeys := resolveCacheKey(logs, job, workdir)
	if len(job.CachePaths) > 0 && cachePolicyRestores(job.CachePolicy) {
		restoreCache(jobCtx, c, logs, job, workdir, restoreCacheKeys)
	}

	logs.printf("$ %s\n", job.Script)
	out, wait, err := exec.Start(jobCtx, job, workdir)
	if err != nil {
		status, exitCode = resolveInterruption("failed", 1)
		if status == "failed" {
			logs.printf("executor error: %v\n", err)
		}
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
		switch {
		case canceledByRequest.Load():
			status = "canceled"
		case jobCtx.Err() == context.DeadlineExceeded:
			status, exitCode = "failed", 124
			logs.printf("\nERROR: job timed out after %s and was killed\n", timeout)
		case execCtx.Err() != nil:
			status = "requeue"
			logs.printf("\nRunner draining; job requeued for another runner\n")
		default:
			logs.printf("\nJob exited with code %d\n", exitCode)
		}
	}

	// Cache save (GitLab-style): after a successful script, when the policy
	// allows a push. Never blocks the job — a store failure is logged and the job
	// still reports success.
	if status == "success" && len(job.CachePaths) > 0 && cachePolicySaves(job.CachePolicy) {
		saveCache(c, logs, job, workdir, primaryCacheKey)
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

// cachePolicyRestores reports whether the policy restores the cache before the
// script (pull, pull-push, or an empty policy which defaults to pull-push).
func cachePolicyRestores(policy string) bool {
	return policy == "" || policy == "pull" || policy == "pull-push"
}

// cachePolicySaves reports whether the policy saves the cache after success
// (push, pull-push, or an empty policy which defaults to pull-push).
func cachePolicySaves(policy string) bool {
	return policy == "" || policy == "push" || policy == "pull-push"
}

// resolveCacheKey turns a job's cache key directives into the primary key (the
// one a save writes to) and the ordered list of keys a restore should try.
//
//   - literal key only:  primary = key (or "default"); restore tries [key].
//   - files: hashing:    primary = "<prefix>-<hash>"; restore tries the exact
//     hashed key first, then the bare prefix as a fallback so a changed lockfile
//     misses cleanly and an unchanged one hits, while a first-ever build can
//     still warm from a previous prefix cache.
//
// The hash is computed here because the workspace files only exist after
// checkout. Returns ("","",nil) shape via empty slice when the job has no cache.
func resolveCacheKey(logs *logStreamer, job *proto.RunnerJob, workdir string) (primary string, restore []string) {
	if len(job.CachePaths) == 0 {
		return "", nil
	}
	base := job.CacheKey
	if base == "" {
		base = "default"
	}
	if len(job.CacheKeyFiles) == 0 {
		return base, []string{base}
	}
	hash := hashCacheFiles(logs, workdir, job.CacheKeyFiles)
	primary = base + "-" + hash
	// Fallback chain: exact content-addressed key, then the bare prefix.
	return primary, []string{primary, base}
}

// hashCacheFiles returns a short hex digest of the listed files' contents
// (relative to workdir), computed deterministically over the sorted file list.
// Missing files are skipped with a log note (they simply don't contribute),
// which still changes the digest when a file appears or disappears because the
// path is only mixed in when present.
func hashCacheFiles(logs *logStreamer, workdir string, files []string) string {
	sorted := append([]string(nil), files...)
	sort.Strings(sorted)
	h := sha256.New()
	for _, f := range sorted {
		clean := strings.TrimSpace(f)
		if clean == "" || strings.HasPrefix(clean, "/") || strings.HasPrefix(clean, "..") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(workdir, clean))
		if err != nil {
			logs.printf("cache: key file %q not found, excluded from hash\n", clean)
			continue
		}
		fmt.Fprintf(h, "%s\x00%d\x00", clean, len(data))
		h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// restoreCache downloads the repo+key cache from the server and untars it into
// the workspace. A miss (404) or any failure is logged and swallowed — the job
// never blocks on the cache. The primary key is passed as ?key= and remaining
// keys as ?fallback= so the server tries the fallback chain.
func restoreCache(ctx context.Context, c *client, logs *logStreamer, job *proto.RunnerJob, workdir string, keys []string) {
	if len(keys) == 0 {
		return
	}
	q := url.Values{}
	q.Set("key", keys[0])
	for _, k := range keys[1:] {
		q.Add("fallback", k)
	}
	rctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet,
		fmt.Sprintf("%s/api/v1/runner/jobs/%d/cache?%s", c.base, job.ID, q.Encode()), nil)
	if err != nil {
		logs.printf("cache: restore request error: %v (continuing)\n", err)
		return
	}
	resp, err := c.do(req)
	if err != nil {
		logs.printf("cache: restore failed: %v (continuing)\n", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		logs.printf("cache miss (key %s)\n", keys[0])
		return
	}
	if resp.StatusCode != http.StatusOK {
		logs.printf("cache: restore returned status %d (continuing)\n", resp.StatusCode)
		return
	}
	cmd := osexec.CommandContext(rctx, "tar", "-xzf", "-", "-C", workdir)
	cmd.Stdin = resp.Body
	if out, err := cmd.CombinedOutput(); err != nil {
		logs.printf("cache: untar failed: %s (continuing)\n", strings.TrimSpace(string(out)))
		return
	}
	matched := resp.Header.Get("X-Cache-Key")
	if matched == "" {
		matched = keys[0]
	}
	logs.printf("cache restored (key %s)\n", matched)
}

// saveCache archives the declared cache paths and uploads them to the server
// keyed by repo+key. Missing paths are skipped. Any failure (including an
// over-cap rejection) is logged and swallowed — the job already succeeded and
// must not be failed by a cache-store problem.
func saveCache(c *client, logs *logStreamer, job *proto.RunnerJob, workdir, key string) {
	if key == "" {
		return
	}
	var existing []string
	for _, p := range job.CachePaths {
		clean := strings.TrimSuffix(strings.TrimSpace(p), "/")
		if clean == "" || strings.HasPrefix(clean, "/") || strings.HasPrefix(clean, "..") {
			logs.printf("cache: skipping unsafe path %q\n", p)
			continue
		}
		if _, err := os.Stat(workdir + "/" + clean); err != nil {
			logs.printf("cache: path %q not found in workspace, skipping\n", clean)
			continue
		}
		existing = append(existing, clean)
	}
	if len(existing) == 0 {
		logs.printf("cache: nothing to save\n")
		return
	}

	archive, err := os.CreateTemp("", "forge-cache-*.tar.gz")
	if err != nil {
		logs.printf("cache: temp file error: %v (continuing)\n", err)
		return
	}
	archive.Close()
	defer os.Remove(archive.Name())

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	tarArgs := append([]string{"-czf", archive.Name(), "-C", workdir}, existing...)
	if out, err := osexec.CommandContext(ctx, "tar", tarArgs...).CombinedOutput(); err != nil {
		logs.printf("cache: tar failed: %v: %s (continuing)\n", err, out)
		return
	}
	f, err := os.Open(archive.Name())
	if err != nil {
		logs.printf("cache: open archive: %v (continuing)\n", err)
		return
	}
	defer f.Close()
	stat, _ := f.Stat()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/api/v1/runner/jobs/%d/cache?key=%s", c.base, job.ID, url.QueryEscape(key)), f)
	if err != nil {
		logs.printf("cache: save request error: %v (continuing)\n", err)
		return
	}
	req.Header.Set("Content-Type", "application/gzip")
	resp, err := c.do(req)
	if err != nil {
		logs.printf("cache: save failed: %v (continuing)\n", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusRequestEntityTooLarge {
		logs.printf("cache: skipped — archive exceeds the server cache size cap (MAX_CACHE_BYTES)\n")
		return
	}
	if resp.StatusCode >= 300 {
		logs.printf("cache: save rejected with status %d (continuing)\n", resp.StatusCode)
		return
	}
	logs.printf("cache saved (key %s, %d bytes: %s)\n", key, stat.Size(), strings.Join(existing, ", "))
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
