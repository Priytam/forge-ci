// forge-runner: pull-based build agent. Long-polls the control plane for
// pending jobs, executes them via the configured executor, streams logs back,
// and heartbeats so the server can detect dead runners.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
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
	"github.com/priytamjeepandey/forge-ci/internal/secrets"
	"github.com/priytamjeepandey/forge-ci/internal/vcs"
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
	// Per-job run directory: the OIDC token file, fetched secrets and the
	// Docker config live here. Mounted read-only into containers.
	if runDir, err := os.MkdirTemp("", "forge-run-*"); err == nil {
		_ = os.Chmod(runDir, 0o755) // readable by a non-root container user
		job.RunDir = runDir
		defer os.RemoveAll(runDir)
	} else {
		logs.printf("warning: could not create the run directory (%v)\n", err)
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

	// Registry logins (registry_login: + DOCKER_AUTH_CONFIG) and external
	// secrets (secrets:) come before the script, from the job's identity. A
	// failure here fails the job: it must not run with half its secrets.
	if err := prepareRegistryAuth(jobCtx, logs, exec, job); err != nil {
		logs.printf("registry login failed: %v\n", err)
		st, code := resolveInterruption("failed", 1)
		finish(st, code)
		return
	}
	if err := fetchSecrets(jobCtx, logs, exec, job); err != nil {
		logs.printf("fetching secrets failed: %v\n", err)
		st, code := resolveInterruption("failed", 1)
		finish(st, code)
		return
	}

	logs.printf("$ %s\n", job.Script)
	// Refreshable OIDC token: keep the job's token in a file and rewrite it
	// before it expires, so credentials built on the file outlive it.
	stopTokens := startTokenRefresh(jobCtx, c, logs, exec, job)
	defer stopTokens()

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
	// JUnit test reports are independent of artifacts.paths: collect and upload
	// them after a successful job so the server can parse pass/fail counts.
	if status == "success" && len(job.ReportJUnitPaths) > 0 {
		uploadJUnitReports(c, logs, job, workdir)
	}
	// Job outputs: the dotenv report (deploy URL/version/digest and variables
	// for later jobs) and a coverage file. Sent before completion, which is
	// when the server writes the deployment record.
	if status == "success" && job.ReportDotenvPath != "" {
		uploadWorkspaceFile(c, logs, job, workdir, job.ReportDotenvPath, "dotenv", "text/plain")
	}
	if status == "success" && job.CoverageReportPath != "" {
		uploadWorkspaceFile(c, logs, job, workdir, job.CoverageReportPath, "coverage", "application/xml")
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

	// CodeCommit arrives without a credential: the control plane cannot embed one
	// because none exists. The runner signs the remote with its OWN AWS identity
	// here, so the credential is minted on the runner, lives for one clone, and
	// never travels over the runner protocol.
	remote := job.CloneURL
	if job.CloneAuth == proto.CloneAuthAWSSigV4 {
		signed, err := signedCodeCommitRemote(cctx, logs, job)
		if err != nil {
			return err
		}
		remote = signed
	}

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
	if err := run("remote", "add", "origin", remote); err != nil {
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

// signedCodeCommitRemote resolves the runner's AWS identity and returns a
// SigV4-signed CodeCommit remote URL for this job.
//
// The identity is keyless by default: the per-job OIDC token Forge already
// injects (FORGE_OIDC_TOKEN) is exchanged for short-lived credentials on the
// repo's role, so the runner holds no AWS key and the role's trust policy can
// condition on the token's repo/ref claims. Operators who would rather use an
// EC2/ECS role set FORGE_AWS_AUTH=instance; FORGE_AWS_ROLE_ARN overrides the
// role the registry supplied.
//
// Every credential component is added to the redaction set BEFORE the signed URL
// is built, so neither the signature nor the session token can reach a log line
// even if git fails and echoes the remote back.
func signedCodeCommitRemote(ctx context.Context, logs *logStreamer, job *proto.RunnerJob) (string, error) {
	repo, ok := vcs.ParseCodeCommitURL(job.CloneURL)
	if !ok {
		return "", fmt.Errorf("codecommit checkout: clone URL %q is not a CodeCommit URL", job.CloneURL)
	}
	if job.AWSRegion != "" {
		repo.Region = job.AWSRegion
	}

	roleARN := job.AWSRoleARN
	if v := strings.TrimSpace(os.Getenv("FORGE_AWS_ROLE_ARN")); v != "" {
		roleARN = v
	}
	forceInstance := strings.EqualFold(strings.TrimSpace(os.Getenv("FORGE_AWS_AUTH")), "instance")
	sessionName := fmt.Sprintf("forge-%d", job.ID)

	creds, source, err := vcs.ResolveRunnerAWSCredentials(
		ctx, repo.Region, roleARN, job.Env["FORGE_OIDC_TOKEN"], sessionName, forceInstance)
	if err != nil {
		return "", fmt.Errorf("codecommit checkout: %w", err)
	}
	// Redact before signing: the signature is derived from the secret, and the
	// session token appears verbatim in the URL's userinfo. job.RedactValues is
	// updated too because cloneSource scrubs git's own output with that list.
	mask := func(values ...string) {
		logs.redact(values...)
		for _, v := range values {
			if v != "" {
				job.RedactValues = append(job.RedactValues, v)
			}
		}
	}
	mask(creds.SecretAccessKey, creds.SessionToken)

	signed, err := vcs.SignCloneURL(repo, creds, time.Now())
	if err != nil {
		return "", fmt.Errorf("codecommit checkout: %w", err)
	}
	mask(signed)
	logs.printf("Authenticated to CodeCommit in %s via %s (no static credentials)\n",
		repo.Region, source)
	return signed, nil
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

// uploadJUnitReports resolves the job's reports.junit globs against the
// workspace, concatenates every matched XML file into one body, and POSTs it to
// the server's report endpoint for parsing. Missing/empty matches and any
// upload failure are logged and swallowed — a report problem never fails a job
// that already succeeded.
func uploadJUnitReports(c *client, logs *logStreamer, job *proto.RunnerJob, workdir string) {
	var combined bytes.Buffer
	matched := 0
	for _, pattern := range job.ReportJUnitPaths {
		clean := strings.TrimSpace(pattern)
		if clean == "" || strings.HasPrefix(clean, "/") || strings.HasPrefix(clean, "..") {
			logs.printf("reports: skipping unsafe junit path %q\n", pattern)
			continue
		}
		files, err := filepath.Glob(filepath.Join(workdir, clean))
		if err != nil {
			logs.printf("reports: bad junit glob %q: %v\n", clean, err)
			continue
		}
		for _, f := range files {
			data, err := os.ReadFile(f)
			if err != nil {
				logs.printf("reports: cannot read %q: %v\n", f, err)
				continue
			}
			combined.Write(data)
			combined.WriteByte('\n')
			matched++
		}
	}
	if matched == 0 {
		logs.printf("reports: no junit files matched\n")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/api/v1/runner/jobs/%d/report", c.base, job.ID), bytes.NewReader(combined.Bytes()))
	if err != nil {
		logs.printf("reports: request error: %v\n", err)
		return
	}
	req.Header.Set("Content-Type", "application/xml")
	resp, err := c.do(req)
	if err != nil {
		logs.printf("reports: upload failed: %v\n", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		logs.printf("reports: upload rejected with status %d\n", resp.StatusCode)
		return
	}
	logs.printf("reports: uploaded %d junit file(s)\n", matched)
}

// prepareRegistryAuth writes the job's Docker config.json (DOCKER_AUTH_CONFIG
// merged with cloud registry logins) into the run directory. The docker
// executor pulls with it; the job sees it as DOCKER_CONFIG, so docker push /
// buildx / crane / skopeo inside the job authenticate too.
func prepareRegistryAuth(ctx context.Context, logs *logStreamer, ex executor.Executor, job *proto.RunnerJob) error {
	if len(job.RegistryLogins) == 0 && job.Env["DOCKER_AUTH_CONFIG"] == "" {
		return nil
	}
	if job.RunDir == "" {
		return errors.New("no run directory for the Docker config")
	}
	cfg, err := secrets.DockerConfig(ctx, secrets.Env(job.Env), job.RegistryLogins)
	if err != nil {
		return err
	}
	if cfg == nil {
		return nil
	}
	dir := filepath.Join(job.RunDir, "docker")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), cfg, 0o644); err != nil {
		return err
	}
	// The minted passwords are secrets: mask them (and their base64 auth forms).
	var parsed struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}
	_ = json.Unmarshal(cfg, &parsed)
	for _, a := range parsed.Auths {
		logs.redact(a.Auth)
		if raw, err := base64.StdEncoding.DecodeString(a.Auth); err == nil {
			if _, pass, ok := strings.Cut(string(raw), ":"); ok && len(pass) >= 8 {
				logs.redact(pass)
			}
		}
	}
	job.DockerConfigDir = dir
	job.Env["DOCKER_CONFIG"] = executor.RunPath(ex, job, "docker")
	delete(job.Env, "DOCKER_AUTH_CONFIG") // consumed; don't hand the raw JSON to the script
	names := make([]string, 0, len(parsed.Auths))
	for r := range parsed.Auths {
		names = append(names, r)
	}
	sort.Strings(names)
	logs.printf("registry login: %s\n", strings.Join(names, ", "))
	return nil
}

// fetchSecrets resolves the job's secrets: block into variables (or files in
// the run directory) and masks every value in the log.
func fetchSecrets(ctx context.Context, logs *logStreamer, ex executor.Executor, job *proto.RunnerJob) error {
	if len(job.Secrets) == 0 {
		return nil
	}
	values, err := secrets.Resolve(ctx, secrets.Env(job.Env), job.Secrets)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(values))
	for name, v := range values {
		names = append(names, name)
		// Same rule as masked variables: a value under 8 characters would
		// redact every occurrence of that string and mangle the log.
		if len(v) >= 8 {
			logs.redact(v)
			job.RedactValues = append(job.RedactValues, v)
		} else if v != "" {
			logs.printf("secrets: %s is shorter than 8 characters and will not be masked in this log\n", name)
		}
		if job.Secrets[name].File {
			if job.RunDir == "" {
				return errors.New("no run directory for secret files")
			}
			dir := filepath.Join(job.RunDir, "secrets")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(dir, name), []byte(v), 0o644); err != nil {
				return err
			}
			job.Env[name] = executor.RunPath(ex, job, "secrets/"+name)
			continue
		}
		job.Env[name] = v
	}
	sort.Strings(names)
	logs.printf("secrets: resolved %s\n", strings.Join(names, ", "))
	return nil
}

// uploadWorkspaceFile posts one workspace file to /runner/jobs/{id}/<kind>.
// A missing file or rejected upload is logged, never a job failure.
func uploadWorkspaceFile(c *client, logs *logStreamer, job *proto.RunnerJob, workdir, rel, kind, contentType string) {
	clean := filepath.Clean(strings.TrimSpace(rel))
	if clean == "." || filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") {
		logs.printf("reports: skipping unsafe %s path %q\n", kind, rel)
		return
	}
	data, err := os.ReadFile(filepath.Join(workdir, clean))
	if err != nil {
		logs.printf("reports: %s file %q not found\n", kind, rel)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/api/v1/runner/jobs/%d/%s", c.base, job.ID, kind), bytes.NewReader(data))
	if err != nil {
		logs.printf("reports: %s request error: %v\n", kind, err)
		return
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := c.do(req)
	if err != nil {
		logs.printf("reports: %s upload failed: %v\n", kind, err)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 300 {
		logs.printf("reports: %s rejected (%d): %s\n", kind, resp.StatusCode, strings.TrimSpace(string(body)))
		return
	}
	logs.printf("reports: %s uploaded: %s\n", kind, strings.TrimSpace(string(body)))
}

// logStreamer batches log bytes and flushes them to the server periodically.
type logStreamer struct {
	c     *client
	jobID int64
	mu    sync.Mutex
	// masked is read on every flush and appended to when a credential is
	// resolved mid-job; both happen under mu.
	masked []string
	buf    bytes.Buffer
	done   chan struct{}
	wg     sync.WaitGroup
}

func newLogStreamer(c *client, jobID int64, redact []string) *logStreamer {
	ls := &logStreamer{c: c, jobID: jobID, masked: redact, done: make(chan struct{})}
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

// redact adds secret-equivalent values to the mask applied to every log chunk.
// Credentials that only exist once a job is running — a CodeCommit clone
// signature, say — are registered here before the value is ever used.
func (ls *logStreamer) redact(values ...string) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	for _, v := range values {
		if v != "" {
			ls.masked = append(ls.masked, v)
		}
	}
}

func (ls *logStreamer) flush() {
	ls.mu.Lock()
	if ls.buf.Len() == 0 {
		ls.mu.Unlock()
		return
	}
	chunk := ls.buf.String()
	ls.buf.Reset()
	// Snapshot under the lock: redact() may append while a flush is in flight.
	masked := make([]string, len(ls.masked))
	copy(masked, ls.masked)
	ls.mu.Unlock()
	for _, v := range masked {
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

// tokenRefreshEvery re-mints the per-job OIDC token well inside its
// 15-minute lifetime.
var tokenRefreshEvery = 5 * time.Minute

const oidcTokenFile = "oidc-token"

// startTokenRefresh writes the job's OIDC token to a file in a per-job run
// directory, points FORGE_OIDC_TOKEN_FILE (and AWS_WEB_IDENTITY_TOKEN_FILE,
// unless the job set it) at it, and refreshes it until the job ends. GCP
// Workload Identity cred-configs (--credential-source-file) and the AWS SDKs
// re-read the file whenever they refresh, so builds longer than the token's
// lifetime keep working. Returns a stop function; a no-op when the job has no
// token (OIDC disabled on the server).
func startTokenRefresh(ctx context.Context, c *client, logs *logStreamer, ex executor.Executor, job *proto.RunnerJob) func() {
	tok := job.Env["FORGE_OIDC_TOKEN"]
	if tok == "" {
		return func() {}
	}
	runDir := job.RunDir
	if runDir == "" { // callers outside runJob (tests): make one here
		dir, err := os.MkdirTemp("", "forge-run-*")
		if err != nil {
			logs.printf("warning: could not create the token directory (%v); FORGE_OIDC_TOKEN_FILE is unavailable\n", err)
			return func() {}
		}
		_ = os.Chmod(dir, 0o755)
		runDir, job.RunDir = dir, dir
	}
	write := func(t string) error {
		tmp := filepath.Join(runDir, oidcTokenFile+".tmp")
		if err := os.WriteFile(tmp, []byte(t), 0o644); err != nil {
			return err
		}
		return os.Rename(tmp, filepath.Join(runDir, oidcTokenFile))
	}
	if err := write(tok); err != nil {
		logs.printf("warning: could not write the token file (%v)\n", err)
		return func() {}
	}
	path := executor.RunPath(ex, job, oidcTokenFile)
	job.Env["FORGE_OIDC_TOKEN_FILE"] = path
	if _, set := job.Env["AWS_WEB_IDENTITY_TOKEN_FILE"]; !set {
		job.Env["AWS_WEB_IDENTITY_TOKEN_FILE"] = path
	}

	rctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(tokenRefreshEvery)
		defer t.Stop()
		for {
			select {
			case <-rctx.Done():
				return
			case <-t.C:
			}
			fresh, err := fetchJobToken(rctx, c, job.ID)
			if err != nil {
				slog.Warn("oidc token refresh failed", "job", job.ID, "err", err)
				continue
			}
			logs.redact(fresh)
			if err := write(fresh); err != nil {
				slog.Warn("oidc token file write failed", "job", job.ID, "err", err)
				continue
			}
			if u, ok := ex.(executor.RunFileUpdater); ok {
				if err := u.UpdateRunFile(rctx, job, oidcTokenFile, []byte(fresh)); err != nil {
					slog.Warn("oidc token push into job failed", "job", job.ID, "err", err)
				}
			}
		}
	}()
	return func() {
		cancel()
		<-done
		os.RemoveAll(runDir)
	}
}

// fetchJobToken asks the server to re-mint the running job's OIDC token.
func fetchJobToken(ctx context.Context, c *client, jobID int64) (string, error) {
	resp, err := c.postJSON(ctx, fmt.Sprintf("/api/v1/runner/jobs/%d/oidc-token", jobID), nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("server returned %d", resp.StatusCode)
	}
	var out proto.OIDCTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.Token == "" {
		return "", errors.New("empty token")
	}
	return out.Token, nil
}
