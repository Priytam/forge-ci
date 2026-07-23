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
	"os/signal"
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
	base string
	http *http.Client
}

func (c *client) post(ctx context.Context, path string, body []byte, contentType string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	return c.http.Do(req)
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
	)
	flag.Parse()

	exec, err := executor.New(*execKind)
	if err != nil {
		slog.Error("bad executor", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	c := &client{base: *server, http: &http.Client{Timeout: 60 * time.Second}}
	slog.Info("forge-runner started", "id", *runnerID, "server", *server, "executor", exec.Name())

	for ctx.Err() == nil {
		job, err := acquire(ctx, c, *runnerID)
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
}

func acquire(ctx context.Context, c *client, runnerID string) (*proto.RunnerJob, error) {
	resp, err := c.postJSON(ctx, "/api/v1/runner/acquire", proto.AcquireRequest{RunnerID: runnerID})
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
	logs := newLogStreamer(c, job.ID)
	logs.printf("Running job #%d %q on %s executor\n$ %s\n", job.ID, job.Name, exec.Name(), job.Script)

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

	status, exitCode := "success", 0
	out, wait, err := exec.Start(ctx, job)
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
		logs.printf("\nJob exited with code %d\n", exitCode)
	}

	stopHB()
	hbWG.Wait()
	logs.close()

	// Report completion with a fresh context: ctx may be canceled on shutdown.
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

// logStreamer batches log bytes and flushes them to the server periodically.
type logStreamer struct {
	c     *client
	jobID int64
	mu    sync.Mutex
	buf   bytes.Buffer
	done  chan struct{}
	wg    sync.WaitGroup
}

func newLogStreamer(c *client, jobID int64) *logStreamer {
	ls := &logStreamer{c: c, jobID: jobID, done: make(chan struct{})}
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
