package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/priytamjeepandey/forge-ci/internal/compiler"
	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

const retryDAG = `
stages: [build, test, deploy]
jobs:
  build:
    stage: build
    script: [make]
  unit:
    stage: test
    needs: [build]
    script: [make test]
  lint:
    stage: test
    needs: [build]
    script: [make lint]
  deploy:
    stage: deploy
    needs: [unit, lint]
    script: [./deploy]
`

func createPipelineT(t *testing.T, st *Store, repo, ref, yml string) *proto.Pipeline {
	t.Helper()
	jobs, err := compiler.Compile(yml, ref, compiler.SourceAPI, nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := st.CreatePipeline(context.Background(), proto.CreatePipelineRequest{
		Repo: repo, Ref: ref, SHA: "abc123", Config: yml,
	}, jobs, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func liveJobs(t *testing.T, st *Store, pid int64) (map[string]proto.Job, string) {
	t.Helper()
	p, js, err := st.GetPipeline(context.Background(), pid)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]proto.Job{}
	for _, j := range js {
		out[j.Name] = j
	}
	return out, p.Status
}

func setStatus(t *testing.T, st *Store, jobID int64, status string) {
	t.Helper()
	if _, err := st.pool.Exec(context.Background(),
		`UPDATE jobs SET status=$2, finished_at=now() WHERE id=$1`, jobID, status); err != nil {
		t.Fatal(err)
	}
}

// TestRetryJobRequeuesFailedJobAndDependents: retrying a failed test job
// re-runs it and the deploy that never ran, inside the same pipeline; the
// successful build and lint are not re-run; needs are rewired to the new rows.
func TestRetryJobRequeuesFailedJobAndDependents(t *testing.T) {
	st := newArtifactsTestStore(t)
	ctx := context.Background()
	p := createPipelineT(t, st, "acme/cos", "main", retryDAG)
	jobs, _ := liveJobs(t, st, p.ID)
	setStatus(t, st, jobs["build"].ID, "success")
	setStatus(t, st, jobs["lint"].ID, "success")
	setStatus(t, st, jobs["unit"].ID, "failed")
	setStatus(t, st, jobs["deploy"].ID, "canceled")
	if _, status := liveJobs(t, st, p.ID); status != "failed" {
		t.Fatalf("pipeline before retry = %s", status)
	}

	if _, err := st.RetryJob(ctx, jobs["build"].ID); !errors.Is(err, ErrNotRetryable) {
		t.Errorf("retrying a successful job: err = %v, want ErrNotRetryable", err)
	}
	res, err := st.RetryJob(ctx, jobs["unit"].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Retried) != 2 || res.Retried[jobs["unit"].ID] == 0 || res.Retried[jobs["deploy"].ID] == 0 {
		t.Fatalf("retried = %v, want unit and deploy", res.Retried)
	}

	after, status := liveJobs(t, st, p.ID)
	if len(after) != 4 {
		t.Fatalf("live jobs = %d, want 4 (old rows hidden)", len(after))
	}
	if after["build"].ID != jobs["build"].ID || after["lint"].ID != jobs["lint"].ID {
		t.Error("successful jobs must not be re-run")
	}
	if after["unit"].ID == jobs["unit"].ID || after["unit"].Status != "created" ||
		after["unit"].RetryOf == nil || *after["unit"].RetryOf != jobs["unit"].ID {
		t.Errorf("unit after retry = %+v", after["unit"])
	}
	if status != "running" {
		t.Errorf("pipeline after retry = %s, want running", status)
	}
	// deploy now needs the NEW unit and the original lint.
	needs := map[int64]bool{}
	for _, n := range after["deploy"].Needs {
		needs[n] = true
	}
	if !needs[after["unit"].ID] || !needs[jobs["lint"].ID] || len(needs) != 2 {
		t.Errorf("deploy needs = %v, want new unit %d + lint %d", after["deploy"].Needs, after["unit"].ID, jobs["lint"].ID)
	}
	// The scheduler runs the new unit (its need, build, succeeded).
	if _, err := st.PromoteReadyJobs(ctx); err != nil {
		t.Fatal(err)
	}
	if j, _ := st.GetJob(ctx, after["unit"].ID); j.Status != "pending" {
		t.Errorf("new unit = %s, want pending", j.Status)
	}
	if j, _ := st.GetJob(ctx, after["deploy"].ID); j.Status != "created" {
		t.Errorf("new deploy = %s, want created (waits for unit)", j.Status)
	}
	// Old rows stay as history, superseded.
	var superseded int
	_ = st.pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE pipeline_id=$1 AND superseded`, p.ID).Scan(&superseded)
	if superseded != 2 {
		t.Errorf("superseded rows = %d, want 2", superseded)
	}
	if _, err := st.RetryJob(ctx, jobs["unit"].ID); !errors.Is(err, ErrNotRetryable) {
		t.Errorf("retrying a superseded job again: %v", err)
	}
}

// TestRetryJobRespectsBranchRules: a job the environment policy refuses on
// this branch cannot be brought back by a retry.
func TestRetryJobRespectsBranchRules(t *testing.T) {
	st := newArtifactsTestStore(t)
	ctx := context.Background()
	yml := "stages: [d]\njobs:\n  ship:\n    stage: d\n    environment: production\n    script: [x]\n"
	if err := st.SaveEnvPolicy(ctx, EnvPolicy{Environments: []RegisteredEnv{
		{Name: "production", Tier: "prod", AllowedRefs: []string{"main"}},
	}}); err != nil {
		t.Fatal(err)
	}
	p := createPipelineT(t, st, "acme/cos", "feature/x", yml)
	jobs, _ := liveJobs(t, st, p.ID)
	if jobs["ship"].Status != "failed" || jobs["ship"].StatusReason == "" {
		t.Fatalf("ship = %+v, want refused", jobs["ship"])
	}
	if _, err := st.RetryJob(ctx, jobs["ship"].ID); !errors.Is(err, ErrRetryRefused) {
		t.Errorf("retry of a refused job: %v, want ErrRetryRefused", err)
	}
}

// TestResourceGroupRunsOneAtATime: two deploys in the same resource group
// (here "deploy-$CI_ENVIRONMENT_NAME" → "deploy-production") never run at
// once; the second is picked up after the first finishes. Other repos and
// groups are unaffected.
func TestResourceGroupRunsOneAtATime(t *testing.T) {
	st := newArtifactsTestStore(t)
	ctx := context.Background()
	yml := `
stages: [d]
jobs:
  ship:
    stage: d
    environment: production
    resource_group: deploy-$CI_ENVIRONMENT_NAME
    script: [./deploy]
`
	p1 := createPipelineT(t, st, "acme/cos", "main", yml)
	p2 := createPipelineT(t, st, "acme/cos", "main", yml)
	p3 := createPipelineT(t, st, "acme/ecom", "main", yml)
	// Release them all as if approved.
	if _, err := st.pool.Exec(ctx, `UPDATE jobs SET status='pending'`); err != nil {
		t.Fatal(err)
	}
	var group string
	_ = st.pool.QueryRow(ctx, `SELECT resource_group FROM jobs WHERE pipeline_id=$1`, p1.ID).Scan(&group)
	if group != "deploy-production" {
		t.Fatalf("resource_group = %q, want deploy-production", group)
	}

	acquire := func() *proto.RunnerJob {
		t.Helper()
		rj, err := st.AcquireJob(ctx, proto.AcquireRequest{RunnerID: "r"})
		if err != nil {
			t.Fatal(err)
		}
		return rj
	}
	first := acquire()
	if first == nil || first.PipelineID != p1.ID {
		t.Fatalf("first acquire = %+v, want pipeline %d", first, p1.ID)
	}
	other := acquire()
	if other == nil || other.PipelineID != p3.ID {
		t.Fatalf("second acquire = %+v, want the other repo's job (pipeline %d)", other, p3.ID)
	}
	if blocked := acquire(); blocked != nil {
		t.Fatalf("a second job of the same group started while the first runs: %+v", blocked)
	}
	if _, _, err := st.CompleteJob(ctx, first.ID, "success", 0); err != nil {
		t.Fatal(err)
	}
	next := acquire()
	if next == nil || next.PipelineID != p2.ID {
		t.Fatalf("after the first finished: %+v, want pipeline %d", next, p2.ID)
	}
}

// TestResourceGroupConcurrentAcquire: many runners polling at once still
// start at most one job of a group.
func TestResourceGroupConcurrentAcquire(t *testing.T) {
	st := newArtifactsTestStore(t)
	ctx := context.Background()
	yml := "stages: [d]\njobs:\n  ship:\n    stage: d\n    resource_group: prod\n    script: [x]\n"
	for i := 0; i < 5; i++ {
		createPipelineT(t, st, "acme/cos", "main", yml)
	}
	if _, err := st.pool.Exec(ctx, `UPDATE jobs SET status='pending'`); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = st.AcquireJob(ctx, proto.AcquireRequest{RunnerID: fmt.Sprintf("r%d", i)})
		}(i)
	}
	wg.Wait()
	var running int
	_ = st.pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE status='running'`).Scan(&running)
	if running != 1 {
		t.Fatalf("running jobs in group = %d, want exactly 1", running)
	}
}
