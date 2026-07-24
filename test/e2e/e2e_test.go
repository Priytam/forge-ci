//go:build e2e

package e2e

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Shared timeouts. Generous so a loaded CI host doesn't flake.
const (
	shortRun  = 60 * time.Second
	medRun    = 90 * time.Second
	dockerRun = 180 * time.Second
)

var repoSeq int64

// uniqueRepo returns a distinct repo name per call so pipelines from different
// subtests never collide (and auto_cancel never crosses tests).
func uniqueRepo(prefix string) string {
	n := atomic.AddInt64(&repoSeq, 1)
	return fmt.Sprintf("e2e-%s-%d-%d", prefix, time.Now().UnixNano(), n)
}

// randSHA returns a random 40-hex-char string standing in for a commit sha.
func randSHA() string {
	b := make([]byte, 20)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func base() string { return stack.baseURL }

// 1. Happy path: DAG order, success, logs (full + ?offset + SSE).
func TestHappyPath(t *testing.T) {
	repo := uniqueRepo("happy")
	config := `
stages: [build, test]
jobs:
  build:
    stage: build
    script:
      - echo building
      - echo BUILD_MARKER
  test:
    stage: test
    script:
      - echo testing
      - echo TEST_MARKER
`
	p := createPipeline(t, base(), repo, "main", randSHA(), config)
	fp, jobs := pollPipeline(t, base(), p.ID, shortRun, pipelineStatusIn("success", "failed", "canceled"))
	if fp.Status != "success" {
		t.Fatalf("pipeline status=%q want success; jobs=%s", fp.Status, describeJobs(jobs))
	}
	build, ok := jobByName(jobs, "build")
	if !ok {
		t.Fatalf("no build job; got %v", jobNames(jobs))
	}
	test, ok := jobByName(jobs, "test")
	if !ok {
		t.Fatalf("no test job; got %v", jobNames(jobs))
	}
	if build.Status != "success" || test.Status != "success" {
		t.Fatalf("jobs not both success: build=%s test=%s", build.Status, test.Status)
	}
	// DAG order: test depends on build (implicit needs = previous stage).
	found := false
	for _, n := range test.Needs {
		if n == build.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("test job needs=%v does not include build id %d", test.Needs, build.ID)
	}

	// Logs: full text.
	full := getJobLogsFull(t, base(), build.ID)
	if !strings.Contains(full, "BUILD_MARKER") {
		t.Fatalf("build full log missing BUILD_MARKER: %q", full)
	}
	// Logs: incremental ?offset delta.
	d := getJobLogsDelta(t, base(), build.ID, 0)
	if !strings.Contains(d.Bytes, "BUILD_MARKER") || !d.EOF {
		t.Fatalf("build log delta unexpected: eof=%v bytes=%q", d.EOF, d.Bytes)
	}
	// A second delta from next_offset should be empty and still EOF.
	d2 := getJobLogsDelta(t, base(), build.ID, d.NextOffset)
	if d2.Bytes != "" || !d2.EOF {
		t.Fatalf("tail delta expected empty+eof, got eof=%v bytes=%q", d2.EOF, d2.Bytes)
	}

	// SSE: connect and confirm it emits the log bytes and a final eof event.
	body, sawEOF := sseCollect(t, base(), build.ID, 20*time.Second)
	if !strings.Contains(body, "BUILD_MARKER") {
		t.Fatalf("SSE body missing BUILD_MARKER: %q", body)
	}
	if !sawEOF {
		t.Fatalf("SSE did not emit an eof event")
	}
}

// 2. Approval gate: protected env blocks; approve → deploys; reject → fails.
func TestApprovalGate(t *testing.T) {
	repo := uniqueRepo("approve")
	// Server-side protected-environment rule (cannot be weakened by YAML).
	mustJSON(t, http.MethodPost, base()+"/api/v1/protected-environments", map[string]any{
		"repo":                repo,
		"name":                "production",
		"required_approvals":  1,
		"allow_self_approval": true,
	}, nil, http.StatusOK)

	config := `
stages: [build, deploy]
jobs:
  build:
    stage: build
    script: [echo built]
  deploy:
    stage: deploy
    environment: production
    script: [echo DEPLOYED]
`

	t.Run("approve", func(t *testing.T) {
		p := createPipeline(t, base(), repo, "main", randSHA(), config)
		// deploy must reach 'blocked' awaiting approval.
		waitJob(t, base(), p.ID, "deploy", shortRun, "blocked")
		dj, _ := jobByName(mustJobs(t, p.ID), "deploy")
		mustJSON(t, http.MethodPost, fmt.Sprintf("%s/api/v1/jobs/%d/approvals", base(), dj.ID),
			map[string]string{"approver": "alice@example.com", "verdict": "approved"}, nil, http.StatusOK)
		j := waitJob(t, base(), p.ID, "deploy", shortRun, "success", "failed", "canceled")
		if j.Status != "success" {
			t.Fatalf("approved deploy status=%q want success", j.Status)
		}
	})

	t.Run("reject", func(t *testing.T) {
		// Different ref so it's an independent pipeline (no auto-cancel of above).
		p := createPipeline(t, base(), repo, "release-1", randSHA(), config)
		waitJob(t, base(), p.ID, "deploy", shortRun, "blocked")
		dj, _ := jobByName(mustJobs(t, p.ID), "deploy")
		mustJSON(t, http.MethodPost, fmt.Sprintf("%s/api/v1/jobs/%d/approvals", base(), dj.ID),
			map[string]string{"approver": "bob@example.com", "verdict": "rejected"}, nil, http.StatusOK)
		j := waitJob(t, base(), p.ID, "deploy", shortRun, "success", "failed", "canceled")
		if j.Status != "failed" {
			t.Fatalf("rejected deploy status=%q want failed", j.Status)
		}
	})
}

// 3. Cancel: a running job canceled is killed; whole-pipeline cancel.
func TestCancel(t *testing.T) {
	longSleep := `
stages: [run]
jobs:
  worker:
    stage: run
    script:
      - echo STARTING
      - sleep 120
      - echo NEVER
`
	t.Run("job", func(t *testing.T) {
		repo := uniqueRepo("cancel-job")
		p := createPipeline(t, base(), repo, "main", randSHA(), longSleep)
		wj := waitJob(t, base(), p.ID, "worker", shortRun, "running")
		mustJSON(t, http.MethodPost, fmt.Sprintf("%s/api/v1/jobs/%d/cancel", base(), wj.ID), nil, nil, http.StatusOK)
		// Cancel is delivered via heartbeat (≤10s), then the runner reports canceled.
		j := waitJob(t, base(), p.ID, "worker", 45*time.Second, "canceled", "failed", "success")
		if j.Status != "canceled" {
			t.Fatalf("job status=%q want canceled", j.Status)
		}
	})

	t.Run("pipeline", func(t *testing.T) {
		repo := uniqueRepo("cancel-pipe")
		p := createPipeline(t, base(), repo, "main", randSHA(), longSleep)
		waitJob(t, base(), p.ID, "worker", shortRun, "running")
		mustJSON(t, http.MethodPost, fmt.Sprintf("%s/api/v1/pipelines/%d/cancel", base(), p.ID), nil, nil, http.StatusOK)
		fp, jobs := pollPipeline(t, base(), p.ID, 45*time.Second, pipelineStatusIn("canceled", "failed", "success"))
		if fp.Status != "canceled" {
			t.Fatalf("pipeline status=%q want canceled; jobs=%s", fp.Status, describeJobs(jobs))
		}
	})
}

// 4. Retry: a job that fails once then succeeds ends success with a retry note.
func TestRetry(t *testing.T) {
	repo := uniqueRepo("retry")
	// The marker lives outside the (per-attempt fresh) workspace so it survives
	// the requeue. First attempt exits 1, second finds the marker and exits 0.
	// The path carries a per-test random nonce so a marker left by an earlier
	// test-e2e run (pipeline ids reset with each throwaway DB) can't be mistaken
	// for this run's — which would skip the failing attempt and break the assert.
	nonce := randSHA()
	config := fmt.Sprintf(`
stages: [test]
jobs:
  flaky:
    stage: test
    retry: 2
    script:
      - 'm="${TMPDIR:-/tmp}/forge-e2e-retry-%s"'
      - 'if [ -f "$m" ]; then echo RETRY_SUCCEEDED; rm -f "$m"; exit 0; fi'
      - 'touch "$m"; echo FIRST_ATTEMPT_FAILING; exit 1'
`, nonce)
	p := createPipeline(t, base(), repo, "main", randSHA(), config)
	j := waitJob(t, base(), p.ID, "flaky", shortRun, "success", "failed", "canceled")
	if j.Status != "success" {
		t.Fatalf("flaky final status=%q want success", j.Status)
	}
	full := getJobLogsFull(t, base(), j.ID)
	if !strings.Contains(full, "attempt 1/3 failed") {
		t.Fatalf("log missing retry separator; got:\n%s", full)
	}
	if !strings.Contains(full, "RETRY_SUCCEEDED") {
		t.Fatalf("log missing second-attempt success marker; got:\n%s", full)
	}
}

// 5. Rules + matrix: matrix expands to N; rules if: include/exclude by ref.
func TestRulesAndMatrix(t *testing.T) {
	t.Run("matrix", func(t *testing.T) {
		repo := uniqueRepo("matrix")
		config := `
stages: [test]
jobs:
  unit:
    stage: test
    script: [echo "go $GOVER"]
    parallel:
      matrix:
        - GOVER: ["1.21", "1.22", "1.23"]
`
		p := createPipeline(t, base(), repo, "main", randSHA(), config)
		fp, jobs := pollPipeline(t, base(), p.ID, shortRun, pipelineStatusIn("success", "failed", "canceled"))
		if fp.Status != "success" {
			t.Fatalf("matrix pipeline status=%q want success; jobs=%s", fp.Status, describeJobs(jobs))
		}
		n := 0
		for _, j := range jobs {
			if strings.HasPrefix(j.Name, "unit") {
				n++
			}
		}
		if n != 3 {
			t.Fatalf("matrix expanded to %d jobs, want 3; got %v", n, jobNames(jobs))
		}
	})

	t.Run("rules_by_ref", func(t *testing.T) {
		repo := uniqueRepo("rules")
		config := `
stages: [deploy]
jobs:
  deploy-prod:
    stage: deploy
    script: [echo prod]
    rules:
      - if: '$CI_COMMIT_BRANCH == "main"'
  deploy-dev:
    stage: deploy
    script: [echo dev]
    rules:
      - if: '$CI_COMMIT_BRANCH != "main"'
`
		pMain := createPipeline(t, base(), repo, "main", randSHA(), config)
		_, mainJobs := getPipeline(t, base(), pMain.ID)
		if _, ok := jobByName(mainJobs, "deploy-prod"); !ok {
			t.Fatalf("main pipeline missing deploy-prod; got %v", jobNames(mainJobs))
		}
		if _, ok := jobByName(mainJobs, "deploy-dev"); ok {
			t.Fatalf("main pipeline should exclude deploy-dev; got %v", jobNames(mainJobs))
		}

		pDev := createPipeline(t, base(), repo, "feature-x", randSHA(), config)
		_, devJobs := getPipeline(t, base(), pDev.ID)
		if _, ok := jobByName(devJobs, "deploy-dev"); !ok {
			t.Fatalf("dev pipeline missing deploy-dev; got %v", jobNames(devJobs))
		}
		if _, ok := jobByName(devJobs, "deploy-prod"); ok {
			t.Fatalf("dev pipeline should exclude deploy-prod; got %v", jobNames(devJobs))
		}
	})
}

// 6. Cache: two runs for the same repo+key; the second restores the first's cache.
func TestCache(t *testing.T) {
	repo := uniqueRepo("cache")
	config := `
stages: [build]
jobs:
  warm:
    stage: build
    cache:
      key: e2e-cache-v1
      paths: [.forge-cache]
    script:
      - 'if [ -f .forge-cache/warm ]; then echo CACHE_HIT; else echo CACHE_MISS; fi'
      - mkdir -p .forge-cache
      - echo warmed > .forge-cache/warm
`
	// Run 1: cold — must save the cache after success.
	p1 := createPipeline(t, base(), repo, "main", randSHA(), config)
	j1 := waitJob(t, base(), p1.ID, "warm", shortRun, "success", "failed", "canceled")
	if j1.Status != "success" {
		t.Fatalf("run1 status=%q want success", j1.Status)
	}
	log1 := getJobLogsFull(t, base(), j1.ID)
	if !strings.Contains(log1, "CACHE_MISS") {
		t.Fatalf("run1 expected CACHE_MISS; got:\n%s", log1)
	}

	// Run 2: same repo+key — must restore and hit. p1 is terminal so auto_cancel
	// does not touch it.
	p2 := createPipeline(t, base(), repo, "main", randSHA(), config)
	j2 := waitJob(t, base(), p2.ID, "warm", shortRun, "success", "failed", "canceled")
	if j2.Status != "success" {
		t.Fatalf("run2 status=%q want success", j2.Status)
	}
	log2 := getJobLogsFull(t, base(), j2.ID)
	if !strings.Contains(log2, "CACHE_HIT") {
		t.Fatalf("run2 expected CACHE_HIT (cache not restored); got:\n%s", log2)
	}
	if !strings.Contains(log2, "cache restored") {
		t.Fatalf("run2 expected runner 'cache restored' line; got:\n%s", log2)
	}
}

// 7. Services (docker-gated): a job reaches a sidecar over the network.
func TestServices(t *testing.T) {
	ctx := context.Background()
	if !dockerAvailable(ctx) {
		t.Skip("SKIP: docker not available (`docker info` failed) — services require the docker executor")
	}
	// A dedicated docker runner tagged 'docker'; the service job is tagged to
	// route to it (the shell runner has no tags and won't pick tagged jobs).
	dr, err := stack.startDockerRunner(ctx, "e2e-docker-svc", []string{"docker"}, nil)
	if err != nil {
		t.Fatalf("start docker runner: %v", err)
	}
	defer dr.kill()

	repo := uniqueRepo("services")
	config := `
stages: [test]
jobs:
  itest:
    stage: test
    image: redis:7-alpine
    tags: [docker]
    services:
      - name: redis:7-alpine
        alias: cachedb
    script:
      - 'until redis-cli -h cachedb ping | grep -q PONG; do echo waiting-for-service; sleep 1; done'
      - echo SERVICE_REACHED
`
	p := createPipeline(t, base(), repo, "main", randSHA(), config)
	j := waitJob(t, base(), p.ID, "itest", dockerRun, "success", "failed", "canceled")
	log := getJobLogsFull(t, base(), j.ID)
	if j.Status != "success" {
		t.Fatalf("services job status=%q want success; log:\n%s", j.Status, log)
	}
	if !strings.Contains(log, "SERVICE_REACHED") {
		t.Fatalf("services job did not reach sidecar; log:\n%s", log)
	}
}

// 8. Artifacts passing: a downstream `needs` restores the upstream artifact.
func TestArtifactsPassing(t *testing.T) {
	repo := uniqueRepo("artifacts")
	config := `
stages: [build, consume]
jobs:
  producer:
    stage: build
    artifacts:
      paths: [out.txt]
    script:
      - echo ARTIFACT_PAYLOAD > out.txt
  consumer:
    stage: consume
    needs: [producer]
    script:
      - cat out.txt
`
	p := createPipeline(t, base(), repo, "main", randSHA(), config)
	fp, jobs := pollPipeline(t, base(), p.ID, shortRun, pipelineStatusIn("success", "failed", "canceled"))
	if fp.Status != "success" {
		t.Fatalf("pipeline status=%q want success; jobs=%s", fp.Status, describeJobs(jobs))
	}
	consumer, ok := jobByName(jobs, "consumer")
	if !ok {
		t.Fatalf("no consumer job; got %v", jobNames(jobs))
	}
	log := getJobLogsFull(t, base(), consumer.ID)
	if !strings.Contains(log, "ARTIFACT_PAYLOAD") {
		t.Fatalf("consumer did not receive upstream artifact; log:\n%s", log)
	}
	if !strings.Contains(log, "Restored artifacts of job") {
		t.Fatalf("consumer missing artifact-restore line; log:\n%s", log)
	}
}

// 9. Variables masking: a masked value is redacted in job logs.
func TestVariableMasking(t *testing.T) {
	repo := uniqueRepo("masking")
	const secret = "supersecretvalue123"
	mustJSON(t, http.MethodPost, base()+"/api/v1/variables", map[string]any{
		"repo":   repo,
		"key":    "SECRET_TOKEN",
		"value":  secret,
		"masked": true,
	}, nil, http.StatusCreated)

	config := `
stages: [test]
jobs:
  leak:
    stage: test
    script:
      - echo "the token is $SECRET_TOKEN done"
`
	p := createPipeline(t, base(), repo, "main", randSHA(), config)
	j := waitJob(t, base(), p.ID, "leak", shortRun, "success", "failed", "canceled")
	if j.Status != "success" {
		t.Fatalf("leak job status=%q want success", j.Status)
	}
	full := getJobLogsFull(t, base(), j.ID)
	if strings.Contains(full, secret) {
		t.Fatalf("masked secret leaked into logs:\n%s", full)
	}
	if !strings.Contains(full, "[MASKED]") {
		t.Fatalf("expected [MASKED] redaction in logs; got:\n%s", full)
	}
}

// 10. Webhook dedup: the same delivery id creates exactly one pipeline.
func TestWebhookDedup(t *testing.T) {
	repo := uniqueRepo("webhook")
	config := `
stages: [build]
jobs:
  build:
    stage: build
    script: [echo built]
`
	// Register the repo's config so the webhook has something to compile.
	mustJSON(t, http.MethodPut, base()+"/api/v1/repo-configs", map[string]string{
		"repo":   repo,
		"config": config,
	}, nil, http.StatusOK)

	sha := randSHA()
	deliveryID := "e2e-delivery-" + randSHA()
	payload := map[string]any{
		"ref":        "refs/heads/main",
		"after":      sha,
		"repository": map[string]string{"full_name": repo},
		"pusher":     map[string]string{"name": "octocat"},
	}
	// First delivery → 201 (created).
	code1 := postGitHubWebhook(t, deliveryID, payload)
	if code1 != http.StatusCreated {
		t.Fatalf("first webhook status=%d want 201", code1)
	}
	// Redelivery with the same id → 200 (deduped, no new pipeline).
	code2 := postGitHubWebhook(t, deliveryID, payload)
	if code2 != http.StatusOK {
		t.Fatalf("duplicate webhook status=%d want 200", code2)
	}
	total := pipelineCount(t, repo)
	if total != 1 {
		t.Fatalf("expected exactly 1 pipeline for %s, got %d", repo, total)
	}
}

// 11. Environments: deploy recorded on the board; rollback creates a pipeline at
// the old sha.
func TestEnvironments(t *testing.T) {
	repo := uniqueRepo("env")
	config := `
stages: [deploy]
jobs:
  deploy-staging:
    stage: deploy
    environment: staging
    script: [echo "deploying $CI_PIPELINE_ID"]
`
	// Register the config so rollback can recompile it.
	mustJSON(t, http.MethodPut, base()+"/api/v1/repo-configs", map[string]string{
		"repo":   repo,
		"config": config,
	}, nil, http.StatusOK)

	shaA := randSHA()
	shaB := randSHA()

	// Deploy A, then B — B becomes the current deployment.
	pA := createPipeline(t, base(), repo, "main", shaA, config)
	if s := waitJob(t, base(), pA.ID, "deploy-staging", shortRun, "success", "failed", "canceled"); s.Status != "success" {
		t.Fatalf("deploy A status=%q want success", s.Status)
	}
	pB := createPipeline(t, base(), repo, "main", shaB, config)
	if s := waitJob(t, base(), pB.ID, "deploy-staging", shortRun, "success", "failed", "canceled"); s.Status != "success" {
		t.Fatalf("deploy B status=%q want success", s.Status)
	}

	// Board: current deployment should be B, with count ≥ 2.
	board := environmentsFor(t, repo)
	staging, ok := boardEnv(board, "staging")
	if !ok {
		t.Fatalf("no staging environment on board: %+v", board)
	}
	if staging.Current == nil || staging.Current.SHA != shaB {
		t.Fatalf("current deployment sha=%v want %s", currentSHA(staging), shaB)
	}
	if staging.DeploymentCount < 2 {
		t.Fatalf("deployment_count=%d want ≥2", staging.DeploymentCount)
	}

	// Rollback to A: creates a NEW pipeline at sha A.
	var rb struct {
		Pipeline pipeline `json:"pipeline"`
	}
	mustJSON(t, http.MethodPost,
		fmt.Sprintf("%s/api/v1/environments/%s/staging/rollback", base(), repo),
		map[string]string{"to_sha": shaA}, &rb, http.StatusCreated)
	if rb.Pipeline.SHA != shaA {
		t.Fatalf("rollback pipeline sha=%q want %s", rb.Pipeline.SHA, shaA)
	}
	if rb.Pipeline.ID == pA.ID || rb.Pipeline.ID == pB.ID {
		t.Fatalf("rollback did not create a NEW pipeline (id=%d)", rb.Pipeline.ID)
	}
	// The rollback pipeline runs through the normal flow to success.
	if s := waitJob(t, base(), rb.Pipeline.ID, "deploy-staging", shortRun, "success", "failed", "canceled"); s.Status != "success" {
		t.Fatalf("rollback deploy status=%q want success", s.Status)
	}
	// Board's current deployment should now be back at A.
	board = environmentsFor(t, repo)
	staging, _ = boardEnv(board, "staging")
	if staging.Current == nil || staging.Current.SHA != shaA {
		t.Fatalf("after rollback current sha=%v want %s", currentSHA(staging), shaA)
	}
}

// 12. Runner auth (RUNNER_AUTH=on): an unauthenticated acquire is rejected 401;
// a token-authenticated one gets past auth.
func TestRunnerAuth(t *testing.T) {
	ctx := context.Background()
	st, err := newServerStack(ctx, "runnerauth", []string{"RUNNER_AUTH=on"})
	if err != nil {
		if st != nil {
			st.dumpLogs(testWriter{t})
			st.stop()
		}
		t.Fatalf("start RUNNER_AUTH=on stack: %v", err)
	}
	defer st.stop()

	// Unauthenticated acquire → 401.
	code, body := doJSON(t, http.MethodPost, st.baseURL+"/api/v1/runner/acquire",
		map[string]any{"runner_id": "no-token", "executor": "shell"})
	if code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated acquire status=%d want 401 body=%s", code, body)
	}

	// Mint a token (admin is open in bootstrap/no-SSO mode).
	var tok struct {
		Token string `json:"token"`
	}
	mustJSON(t, http.MethodPost, st.baseURL+"/api/v1/runner-tokens",
		map[string]string{"description": "e2e"}, &tok, http.StatusCreated)
	if tok.Token == "" {
		t.Fatalf("runner-tokens did not return a token")
	}

	// Authenticated acquire gets PAST auth: it either long-polls (client timeout)
	// or returns 204/200 — anything but 401 proves the token was accepted.
	client := &http.Client{Timeout: 3 * time.Second}
	req, _ := http.NewRequest(http.MethodPost, st.baseURL+"/api/v1/runner/acquire",
		strings.NewReader(`{"runner_id":"tok","executor":"shell"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	resp, err := client.Do(req)
	if err != nil {
		// A client-timeout means the server accepted the token and is long-polling
		// for a job — auth passed.
		if isTimeout(err) {
			return
		}
		t.Fatalf("authenticated acquire error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		t.Fatalf("authenticated acquire was rejected 401 despite a valid token")
	}
}

// TestSelfCIConfigCompiles proves the repo's own .forge-ci.yml is a valid Forge
// pipeline by registering it through PUT /repo-configs, which compiles it with
// the real compiler (rejecting broken YAML with 400). It then compiles for both
// a branch and main ref by creating pipelines, asserting the expected jobs.
func TestSelfCIConfigCompiles(t *testing.T) {
	path := filepath.Join(repoRoot, ".forge-ci.yml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	cfg := string(raw)
	repo := uniqueRepo("selfci")

	// Route this repo's jobs to a tag no runner has, so compiling the config into
	// a real pipeline never causes the shell runner to run go/npm builds.
	mustJSON(t, http.MethodPut, base()+"/api/v1/repo-settings", map[string]any{
		"repo":                repo,
		"default_runner_tags": []string{"selfci-norun"},
	}, nil, http.StatusNoContent)

	// PUT validates by compiling; a parse/compile error would be 400.
	mustJSON(t, http.MethodPut, base()+"/api/v1/repo-configs", map[string]string{
		"repo":   repo,
		"config": cfg,
	}, nil, http.StatusOK)

	// Compile it into a real DAG and assert the expected job set. Every job runs
	// on every ref (no rules/only/except), so all are present.
	p := createPipeline(t, base(), repo, "main", randSHA(), cfg)
	_, jobs := getPipeline(t, base(), p.ID)
	for _, want := range []string{"build", "unit-tests", "vet", "gofmt", "web-build"} {
		if _, ok := jobByName(jobs, want); !ok {
			t.Fatalf("self-CI config missing job %q; got %v", want, jobNames(jobs))
		}
	}
}

// ---- test-local helpers ----

func mustJobs(t *testing.T, pid int64) []job {
	t.Helper()
	_, jobs := getPipeline(t, base(), pid)
	return jobs
}

func postGitHubWebhook(t *testing.T, deliveryID string, payload any) int {
	t.Helper()
	code, _ := doWithHeaders(t, http.MethodPost, base()+"/api/v1/webhooks/github", payload, map[string]string{
		"X-GitHub-Event":    "push",
		"X-GitHub-Delivery": deliveryID,
	})
	return code
}

// pipelineCount reads X-Total-Count for a repo's pipeline list.
func pipelineCount(t *testing.T, repo string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, base()+"/api/v1/pipelines?repo="+repo, nil)
	resp, err := httpc.Do(req)
	if err != nil {
		t.Fatalf("list pipelines: %v", err)
	}
	defer resp.Body.Close()
	drainReader(resp.Body)
	n, _ := strconv.Atoi(resp.Header.Get("X-Total-Count"))
	return n
}

// deployment / environment board shapes (local, HTTP-only).
type deployment struct {
	SHA         string `json:"sha"`
	Environment string `json:"environment"`
	PipelineID  int64  `json:"pipeline_id"`
}

type envBoard struct {
	Environment     string      `json:"environment"`
	Current         *deployment `json:"current"`
	DeploymentCount int         `json:"deployment_count"`
	Drift           string      `json:"drift"`
}

func environmentsFor(t *testing.T, repo string) []envBoard {
	t.Helper()
	var out []envBoard
	mustJSON(t, http.MethodGet, base()+"/api/v1/environments?repo="+repo, nil, &out, http.StatusOK)
	return out
}

func boardEnv(board []envBoard, env string) (envBoard, bool) {
	for _, b := range board {
		if b.Environment == env {
			return b, true
		}
	}
	return envBoard{}, false
}

func currentSHA(b envBoard) string {
	if b.Current == nil {
		return "<nil>"
	}
	return b.Current.SHA
}

// doWithHeaders is doJSON with extra request headers.
func doWithHeaders(t *testing.T, method, urlStr string, body any, headers map[string]string) (int, []byte) {
	t.Helper()
	var rdr *strings.Reader
	if body != nil {
		b := mustMarshal(t, body)
		rdr = strings.NewReader(string(b))
	}
	var req *http.Request
	var err error
	if rdr != nil {
		req, err = http.NewRequest(method, urlStr, rdr)
	} else {
		req, err = http.NewRequest(method, urlStr, nil)
	}
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpc.Do(req)
	if err != nil {
		t.Fatalf("do %s %s: %v", method, urlStr, err)
	}
	defer resp.Body.Close()
	out := mustReadAll(t, resp)
	return resp.StatusCode, out
}

func isTimeout(err error) bool {
	type timeouter interface{ Timeout() bool }
	if te, ok := err.(timeouter); ok && te.Timeout() {
		return true
	}
	// url.Error wraps net timeouts.
	return strings.Contains(err.Error(), "context deadline exceeded") ||
		strings.Contains(err.Error(), "Client.Timeout")
}

// testWriter adapts *testing.T to io.Writer for dumpLogs on setup failure.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(string(p))
	return len(p), nil
}
