package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/priytamjeepandey/forge-ci/internal/compiler"
	"github.com/priytamjeepandey/forge-ci/internal/proto"
	"github.com/priytamjeepandey/forge-ci/internal/store"
	"github.com/priytamjeepandey/forge-ci/internal/vcs"
)

// ---- throwaway-DB harness -------------------------------------------------

func baseDSN() string {
	if v := os.Getenv("DATABASE_URL"); v != "" {
		return v
	}
	return "postgres://forge:forge@localhost:5433/forge?sslmode=disable"
}

// newTestStore creates a fresh throwaway database, runs migrations into it via
// store.New, and drops it on cleanup. Skips the test if Postgres is unreachable.
func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()

	admin, err := pgx.Connect(ctx, baseDSN())
	if err != nil {
		t.Skipf("postgres not reachable (%v); skipping DB-backed status test", err)
	}
	dbName := fmt.Sprintf("forge_cs_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		admin.Close(ctx)
		t.Skipf("cannot create throwaway db (%v); skipping", err)
	}

	u, _ := url.Parse(baseDSN())
	u.Path = "/" + dbName
	st, err := store.New(ctx, u.String())
	if err != nil {
		_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName+" WITH (FORCE)")
		admin.Close(ctx)
		t.Fatalf("store.New on throwaway db: %v", err)
	}
	t.Cleanup(func() {
		st.Close()
		_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName+" WITH (FORCE)")
		admin.Close(ctx)
	})
	return st
}

// ---- recording stub -------------------------------------------------------

type stubReq struct {
	path string
	auth string
	body map[string]string
}

type statusStub struct {
	mu     sync.Mutex
	reqs   []stubReq
	status int // response code to return (default 201)
}

func newStatusStub() (*statusStub, *httptest.Server) {
	s := &statusStub{status: http.StatusCreated}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]string
		_ = json.Unmarshal(b, &body)
		s.mu.Lock()
		s.reqs = append(s.reqs, stubReq{path: r.URL.Path, auth: r.Header.Get("Authorization"), body: body})
		code := s.status
		s.mu.Unlock()
		w.WriteHeader(code)
	}))
	return s, srv
}

func (s *statusStub) all() []stubReq {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]stubReq, len(s.reqs))
	copy(out, s.reqs)
	return out
}

func (s *statusStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.reqs)
}

// ---- helpers --------------------------------------------------------------

// newStatusScheduler builds a Scheduler wired only for status posting, with its
// poster pointed at the stub. Fast retry backoff keeps tests quick.
func newStatusScheduler(t *testing.T, st *store.Store, githubBase string) *Scheduler {
	t.Helper()
	t.Setenv("GITHUB_API_BASE", githubBase)
	return &Scheduler{
		store:         st,
		poster:        vcs.NewPoster(),
		statusEnabled: true,
		statusSem:     make(chan struct{}, 4),
		inflight:      map[string]struct{}{},
	}
}

// tick runs one status-posting pass and waits for its async deliveries to finish.
func (sc *Scheduler) runTick(t *testing.T, ctx context.Context) {
	t.Helper()
	sc.postStatuses(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		sc.inflightMu.Lock()
		n := len(sc.inflight)
		sc.inflightMu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for status posts to drain")
}

func makePipeline(t *testing.T, ctx context.Context, st *store.Store, repo, sha string) *proto.Pipeline {
	t.Helper()
	jobs := []compiler.CompiledJob{{Name: "build", Stage: "build", StageIdx: 0, Script: "echo hi"}}
	p, err := st.CreatePipeline(ctx,
		proto.CreatePipelineRequest{Repo: repo, Ref: "main", SHA: sha, Config: "jobs:\n  build:\n    script: echo hi"},
		jobs, nil, false, false)
	if err != nil {
		t.Fatalf("CreatePipeline: %v", err)
	}
	return p
}

// ---- tests ----------------------------------------------------------------

// TestStatusSequence drives a pipeline created -> promoted -> running -> success
// and asserts the stub received a pending, a running and a success post — each
// with the right sha in the path, context/target_url/auth — and that repeat
// ticks do not re-post the same (sha, status).
func TestStatusSequence(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	stub, srv := newStatusStub()
	defer srv.Close()
	sc := newStatusScheduler(t, st, srv.URL)

	const repo, sha, token = "acme/checkout", "deadbeefcafefeed", "ghp_testtoken"
	if err := st.RegisterRepo(ctx, proto.RepoRegistration{
		Repo: repo, Provider: "github", CloneURL: "https://github.com/" + repo + ".git",
		Token: token, DefaultBranch: "main",
	}); err != nil {
		t.Fatalf("RegisterRepo: %v", err)
	}

	p := makePipeline(t, ctx, st, repo, sha)

	// Tick 1: jobs are all 'created' -> pending post.
	sc.runTick(t, ctx)
	// A duplicate tick with unchanged state must NOT re-post.
	sc.runTick(t, ctx)

	// Promote created -> pending -> current phase becomes "running".
	if _, err := st.PromoteReadyJobs(ctx); err != nil {
		t.Fatalf("PromoteReadyJobs: %v", err)
	}
	sc.runTick(t, ctx)
	sc.runTick(t, ctx) // dedupe

	// Acquire the job (pending -> running); phase is still "running" (dedupe).
	job, err := st.AcquireJob(ctx, proto.AcquireRequest{RunnerID: "r1", Executor: "shell"})
	if err != nil || job == nil {
		t.Fatalf("AcquireJob: job=%v err=%v", job, err)
	}
	sc.runTick(t, ctx) // dedupe (running already posted)

	// Complete the job -> success.
	if _, _, err := st.CompleteJob(ctx, job.ID, "success", 0); err != nil {
		t.Fatalf("CompleteJob: %v", err)
	}
	sc.runTick(t, ctx)
	sc.runTick(t, ctx) // dedupe

	reqs := stub.all()
	if len(reqs) != 3 {
		t.Fatalf("got %d posts, want exactly 3 (pending, running, success):\n%+v", len(reqs), reqs)
	}

	wantPath := fmt.Sprintf("/repos/%s/statuses/%s", repo, sha)
	wantStates := []string{"pending", "pending", "success"} // running maps to pending
	wantTarget := fmt.Sprintf("/pipelines/%d", p.ID)
	for i, r := range reqs {
		if r.path != wantPath {
			t.Errorf("post %d path = %q, want %q", i, r.path, wantPath)
		}
		if r.auth != "Bearer "+token {
			t.Errorf("post %d auth = %q, want bearer with token", i, r.auth)
		}
		if r.body["context"] != vcs.StatusContext {
			t.Errorf("post %d context = %q, want %q", i, r.body["context"], vcs.StatusContext)
		}
		if !contains(r.body["target_url"], wantTarget) {
			t.Errorf("post %d target_url = %q, want it to contain %q", i, r.body["target_url"], wantTarget)
		}
		if r.body["state"] != wantStates[i] {
			t.Errorf("post %d state = %q, want %q", i, r.body["state"], wantStates[i])
		}
	}
	t.Logf("posts: %d states=%v path=%s target=%s", len(reqs),
		[]string{reqs[0].body["state"], reqs[1].body["state"], reqs[2].body["state"]}, reqs[0].path, reqs[0].body["target_url"])
}

// TestNoTokenAndOtherProviderPostNothing verifies repos that are unpostable
// (github but no token, and provider "other") never generate a post.
func TestNoTokenAndOtherProviderPostNothing(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	stub, srv := newStatusStub()
	defer srv.Close()
	sc := newStatusScheduler(t, st, srv.URL)

	// github repo with NO token.
	if err := st.RegisterRepo(ctx, proto.RepoRegistration{
		Repo: "acme/notoken", Provider: "github",
		CloneURL: "https://github.com/acme/notoken.git", DefaultBranch: "main",
	}); err != nil {
		t.Fatalf("RegisterRepo notoken: %v", err)
	}
	// provider "other" with a token (no status API).
	if err := st.RegisterRepo(ctx, proto.RepoRegistration{
		Repo: "acme/other", Provider: "other",
		CloneURL: "https://git.example.com/acme/other.git", Token: "tok", DefaultBranch: "main",
	}); err != nil {
		t.Fatalf("RegisterRepo other: %v", err)
	}

	makePipeline(t, ctx, st, "acme/notoken", "sha1")
	makePipeline(t, ctx, st, "acme/other", "sha2")

	sc.runTick(t, ctx)
	if _, err := st.PromoteReadyJobs(ctx); err != nil {
		t.Fatalf("PromoteReadyJobs: %v", err)
	}
	sc.runTick(t, ctx)

	if n := stub.count(); n != 0 {
		t.Fatalf("expected 0 posts for no-token / provider=other repos, got %d: %+v", n, stub.all())
	}
}

// TestPersistent401KeepsClaimAndIsNotFatal verifies a 401 from the VCS is
// retried per policy, logged, non-fatal, and the dedup claim is KEPT (so the
// scheduler does not hammer a misconfigured repo).
func TestPersistent401KeepsClaimAndIsNotFatal(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	stub, srv := newStatusStub()
	defer srv.Close()
	stub.mu.Lock()
	stub.status = http.StatusUnauthorized
	stub.mu.Unlock()
	sc := newStatusScheduler(t, st, srv.URL)

	const repo, sha = "acme/checkout", "cafebabe"
	if err := st.RegisterRepo(ctx, proto.RepoRegistration{
		Repo: repo, Provider: "github", CloneURL: "https://github.com/" + repo + ".git",
		Token: "badtoken", DefaultBranch: "main",
	}); err != nil {
		t.Fatalf("RegisterRepo: %v", err)
	}
	p := makePipeline(t, ctx, st, repo, sha)

	sc.runTick(t, ctx) // pending post -> 401 -> retried, given up, claim kept

	if n := stub.count(); n < 2 {
		t.Fatalf("expected the 401 to be retried (>=2 requests), got %d", n)
	}
	// Claim must still be held: a fresh claim attempt for the same (pipeline,
	// status="pending") should fail because the permanent-failure path kept it.
	claimed, err := st.ClaimStatusPost(ctx, p.ID, "pending")
	if err != nil {
		t.Fatalf("ClaimStatusPost: %v", err)
	}
	if claimed {
		t.Error("claim was released after a permanent 401; expected it to be kept")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
