//go:build e2e

// Package e2e is a black-box, end-to-end integration suite for Forge CI.
//
// It exercises the REAL HTTP surface: TestMain builds the forge-server and
// forge-runner binaries, provisions an ISOLATED throwaway Postgres database,
// starts a server on a spare port and a shell-executor runner, waits for
// health, and tears everything down after. The subtests in e2e_test.go drive
// pipelines through the public API and assert real outcomes by polling.
//
// It complements (does not replace) the per-package unit tests: those call
// internal funcs directly against a DB; this one only speaks HTTP.
//
// Run it with:  make test-e2e   (i.e. go test -tags e2e ./test/e2e/...)
//
// Requirements: Postgres reachable at DATABASE_URL (default the docker-compose
// instance on :5433). If Postgres is unavailable the suite skips with a clear
// message instead of failing. Docker-dependent subtests self-skip on `docker
// info` failure.
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// ---- global harness state (owned by TestMain) ----

var (
	repoRoot  string // module root (…/forge-ci)
	serverBin string // built forge-server binary
	runnerBin string // built forge-runner binary
	adminDSN  string // DSN of an existing admin DB (to CREATE/DROP the throwaway DB)

	stack *serverStack // the main core-tests stack (server + shell runner + its DB)
)

// httpc is a shared client with a generous timeout (SSE uses its own).
var httpc = &http.Client{Timeout: 30 * time.Second}

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

// run does setup, runs the suite, and tears down — returning the exit code.
// Split out so deferred teardown runs before os.Exit.
func run(m *testing.M) int {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	root, err := findRepoRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: cannot locate repo root: %v\n", err)
		return 1
	}
	repoRoot = root

	adminDSN = firstNonEmpty(os.Getenv("E2E_DATABASE_URL"), os.Getenv("DATABASE_URL"),
		"postgres://forge:forge@localhost:5433/forge?sslmode=disable")

	// Preflight: is Postgres reachable? If not, SKIP the whole suite (exit 0) with
	// a clear message — the suite must not fail merely because infra is absent.
	if err := pingPostgres(ctx, adminDSN); err != nil {
		fmt.Fprintf(os.Stderr,
			"\ne2e: SKIPPING — Postgres is not reachable at %s (%v).\n"+
				"     Start it with `docker compose up -d postgres` (host :5433), or set\n"+
				"     E2E_DATABASE_URL, then re-run `make test-e2e`.\n\n",
			redactDSN(adminDSN), err)
		return 0
	}

	// Build the real binaries once.
	binDir, err := os.MkdirTemp("", "forge-e2e-bin-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: mktemp: %v\n", err)
		return 1
	}
	defer os.RemoveAll(binDir)
	serverBin = filepath.Join(binDir, "forge-server")
	runnerBin = filepath.Join(binDir, "forge-runner")
	if err := goBuild(ctx, serverBin, "./cmd/server"); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: build forge-server: %v\n", err)
		return 1
	}
	if err := goBuild(ctx, runnerBin, "./cmd/runner"); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: build forge-runner: %v\n", err)
		return 1
	}

	// Bring up the main stack for the core tests: a server on LOG_BACKEND=postgres
	// (so most tests need only Postgres, no Redis) plus a shell runner.
	stack, err = newServerStack(ctx, "core", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: start core stack: %v\n", err)
		if stack != nil {
			stack.dumpLogs(os.Stderr)
			stack.stop()
		}
		return 1
	}
	defer stack.stop()

	if err := stack.startShellRunner(ctx, "e2e-shell", nil); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: start shell runner: %v\n", err)
		stack.dumpLogs(os.Stderr)
		return 1
	}

	return m.Run()
}

// ---- server stack ----

// serverStack is one running forge-server plus its isolated database and any
// runners it spawned. The core suite uses one; some subtests (e.g. runner-auth)
// spin their own.
type serverStack struct {
	name    string
	baseURL string
	dbName  string
	dbDSN   string

	server  *proc
	runners []*proc
}

// newServerStack provisions a throwaway DB, starts a server on a free port with
// the given extra env, and waits for health.
func newServerStack(ctx context.Context, name string, extraEnv []string) (*serverStack, error) {
	dbName, dbDSN, err := createDatabase(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("create database: %w", err)
	}
	st := &serverStack{name: name, dbName: dbName, dbDSN: dbDSN}

	port, err := freePort()
	if err != nil {
		return nil, err
	}
	st.baseURL = fmt.Sprintf("http://127.0.0.1:%d", port)

	env := append([]string{
		"DATABASE_URL=" + dbDSN,
		"LISTEN_ADDR=" + fmt.Sprintf(":%d", port),
		"LOG_BACKEND=postgres", // core tests avoid a hard Redis dependency
		"COMMIT_STATUS=off",    // no outbound VCS calls in tests
	}, extraEnv...)

	p, err := startProc(ctx, "server-"+name, serverBin, nil, env)
	if err != nil {
		return nil, fmt.Errorf("start server: %w", err)
	}
	st.server = p

	if err := waitHealthy(ctx, st.baseURL, 30*time.Second); err != nil {
		return st, fmt.Errorf("server never became healthy: %w", err)
	}
	return st, nil
}

// startShellRunner starts a shell-executor runner attached to this stack.
func (st *serverStack) startShellRunner(ctx context.Context, id string, extraEnv []string) error {
	args := []string{"--executor=shell", "--server=" + st.baseURL, "--id=" + id}
	env := append([]string{"RUNNER_DRAIN_GRACE=1s"}, extraEnv...)
	p, err := startProc(ctx, "runner-"+id, runnerBin, args, env)
	if err != nil {
		return err
	}
	st.runners = append(st.runners, p)
	return nil
}

// startDockerRunner starts a docker-executor runner with the given tags. Used by
// docker-gated subtests; the tags keep its jobs separate from the shell runner.
func (st *serverStack) startDockerRunner(ctx context.Context, id string, tags []string, extraEnv []string) (*proc, error) {
	args := []string{"--executor=docker", "--server=" + st.baseURL, "--id=" + id}
	if len(tags) > 0 {
		args = append(args, "--tags="+strings.Join(tags, ","))
	}
	env := append([]string{"RUNNER_DRAIN_GRACE=1s"}, extraEnv...)
	p, err := startProc(ctx, "runner-"+id, runnerBin, args, env)
	if err != nil {
		return nil, err
	}
	st.runners = append(st.runners, p)
	return p, nil
}

// stop kills every runner and the server (process groups), then drops the DB.
func (st *serverStack) stop() {
	for _, r := range st.runners {
		r.kill()
	}
	if st.server != nil {
		st.server.kill()
	}
	// Drop the throwaway DB with a fresh context (the suite's may be done).
	dctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := dropDatabase(dctx, st.dbName); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: drop database %s: %v\n", st.dbName, err)
	}
}

// dumpLogs writes captured server + runner output to w (for debugging failures).
func (st *serverStack) dumpLogs(w io.Writer) {
	if st.server != nil {
		fmt.Fprintf(w, "\n----- %s server log -----\n%s\n", st.name, st.server.output())
	}
	for _, r := range st.runners {
		fmt.Fprintf(w, "\n----- %s log -----\n%s\n", r.name, r.output())
	}
}

// ---- process management ----

// proc is a spawned child process with captured, capped output.
type proc struct {
	name string
	cmd  *exec.Cmd
	buf  *capBuffer
}

func startProc(ctx context.Context, name, bin string, args, extraEnv []string) (*proc, error) {
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), extraEnv...)
	cmd.Dir = repoRoot
	// Own process group so we can kill the whole tree (executor subprocesses too).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	buf := newCapBuffer(256 * 1024)
	cmd.Stdout = buf
	cmd.Stderr = buf
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &proc{name: name, cmd: cmd, buf: buf}, nil
}

func (p *proc) kill() {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return
	}
	// Kill the whole process group.
	_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
	_ = p.cmd.Process.Kill()
	_, _ = p.cmd.Process.Wait()
}

func (p *proc) output() string { return p.buf.String() }

// capBuffer is a threadsafe, size-capped ring-ish buffer: once full it keeps the
// most recent bytes (drops the oldest) so a chatty process can't blow up memory.
type capBuffer struct {
	mu  chanMutex
	max int
	b   bytes.Buffer
}

func newCapBuffer(max int) *capBuffer { return &capBuffer{max: max, mu: newChanMutex()} }

func (c *capBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.b.Write(p)
	if c.b.Len() > c.max {
		// Trim to the last c.max bytes.
		data := c.b.Bytes()
		keep := append([]byte(nil), data[len(data)-c.max:]...)
		c.b.Reset()
		c.b.Write(keep)
	}
	return len(p), nil
}

func (c *capBuffer) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.b.String()
}

// chanMutex is a tiny mutex built on a buffered channel (avoids importing sync
// just for one lock; keeps this file dependency-light).
type chanMutex chan struct{}

func newChanMutex() chanMutex { m := make(chanMutex, 1); return m }
func (m chanMutex) Lock()     { m <- struct{}{} }
func (m chanMutex) Unlock()   { <-m }

// ---- database provisioning ----

// createDatabase creates an isolated throwaway database and returns its name and
// DSN. The name is unique per stack + pid + timestamp.
func createDatabase(ctx context.Context, tag string) (name, dsn string, err error) {
	name = fmt.Sprintf("forge_e2e_%s_%d_%d", sanitize(tag), os.Getpid(), time.Now().UnixNano())
	conn, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		return "", "", err
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+quoteIdent(name)); err != nil {
		return "", "", err
	}
	dsn, err = dsnWithDB(adminDSN, name)
	if err != nil {
		return "", "", err
	}
	return name, dsn, nil
}

// dropDatabase terminates connections and drops the throwaway database.
func dropDatabase(ctx context.Context, name string) error {
	conn, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	// Terminate other connections so DROP doesn't block.
	_, _ = conn.Exec(ctx,
		`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=$1 AND pid<>pg_backend_pid()`,
		name)
	_, err = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+quoteIdent(name))
	return err
}

func pingPostgres(ctx context.Context, dsn string) error {
	c, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := pgx.Connect(c, dsn)
	if err != nil {
		return err
	}
	defer conn.Close(c)
	return conn.Ping(c)
}

// ---- small helpers ----

func findRepoRoot() (string, error) {
	// Walk up from CWD until we find go.mod.
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found from working dir")
		}
		dir = parent
	}
}

func goBuild(ctx context.Context, out, pkg string) error {
	cmd := exec.CommandContext(ctx, "go", "build", "-o", out, pkg)
	cmd.Dir = repoRoot
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, b)
	}
	return nil
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func waitHealthy(ctx context.Context, baseURL string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/v1/healthz", nil)
		resp, err := httpc.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("timeout after %s", timeout)
}

func dsnWithDB(dsn, db string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	u.Path = "/" + db
	return u.String(), nil
}

func redactDSN(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return dsn
	}
	if u.User != nil {
		u.User = url.UserPassword(u.User.Username(), "xxx")
	}
	return u.String()
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	return b.String()
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// dockerAvailable reports whether `docker info` succeeds (used to gate
// docker-executor subtests).
func dockerAvailable(ctx context.Context) bool {
	c, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return exec.CommandContext(c, "docker", "info").Run() == nil
}

// drainReader reads and discards a reader (used to reuse keep-alive conns).
func drainReader(r io.Reader) { _, _ = io.Copy(io.Discard, bufio.NewReader(r)) }
