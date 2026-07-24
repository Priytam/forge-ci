package store

import (
	"context"
	"testing"

	"github.com/priytamjeepandey/forge-ci/internal/compiler"
	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

// twoParallelJobs is a build-stage pair with no needs between them, the shape
// fail-fast is meant to protect: one fails, the sibling should be stopped.
func twoParallelJobs() []compiler.CompiledJob {
	return []compiler.CompiledJob{
		{Name: "a", Stage: "build", StageIdx: 0, Script: "false", Env: map[string]string{}},
		{Name: "b", Stage: "build", StageIdx: 0, Script: "sleep 60", Env: map[string]string{}},
	}
}

func makeFFPipeline(t *testing.T, st *Store, failFast bool, jobs []compiler.CompiledJob) *proto.Pipeline {
	t.Helper()
	ctx := context.Background()
	p, err := st.CreatePipeline(ctx, proto.CreatePipelineRequest{
		Repo: "acme/ff", Ref: "main", SHA: "deadbeef", Config: "x",
	}, jobs, nil, false, failFast)
	if err != nil {
		t.Fatalf("CreatePipeline: %v", err)
	}
	return p
}

// setJobStatus forces a job (by name, within a pipeline) into a state so the
// transition can be exercised without a live runner.
func setJobStatus(t *testing.T, st *Store, pipelineID int64, name, status string, allowFailure bool) int64 {
	t.Helper()
	ctx := context.Background()
	var id int64
	err := st.pool.QueryRow(ctx,
		`UPDATE jobs SET status=$3, allow_failure=$4 WHERE pipeline_id=$1 AND name=$2 RETURNING id`,
		pipelineID, name, status, allowFailure).Scan(&id)
	if err != nil {
		t.Fatalf("setJobStatus(%s=%s): %v", name, status, err)
	}
	return id
}

func jobState(t *testing.T, st *Store, id int64) (status string, cancelReq bool) {
	t.Helper()
	if err := st.pool.QueryRow(context.Background(),
		`SELECT status, cancel_requested FROM jobs WHERE id=$1`, id).Scan(&status, &cancelReq); err != nil {
		t.Fatalf("jobState(%d): %v", id, err)
	}
	return
}

// A genuine failure of a non-allow_failure job cancels created/pending/blocked
// siblings outright and flags a running sibling for cancellation.
func TestFailFastCancel_Triggers(t *testing.T) {
	st := newDeployTestStore(t)
	jobs := []compiler.CompiledJob{
		{Name: "failed", Stage: "build", StageIdx: 0, Script: "false", Env: map[string]string{}},
		{Name: "running", Stage: "build", StageIdx: 0, Script: "sleep 60", Env: map[string]string{}},
		{Name: "created", Stage: "build", StageIdx: 0, Script: "echo", Env: map[string]string{}},
		{Name: "pending", Stage: "build", StageIdx: 0, Script: "echo", Env: map[string]string{}},
	}
	p := makeFFPipeline(t, st, true, jobs)
	failedID := setJobStatus(t, st, p.ID, "failed", "failed", false)
	runningID := setJobStatus(t, st, p.ID, "running", "running", false)
	createdID := setJobStatus(t, st, p.ID, "created", "created", false)
	pendingID := setJobStatus(t, st, p.ID, "pending", "pending", false)

	n, err := st.FailFastCancel(context.Background())
	if err != nil {
		t.Fatalf("FailFastCancel: %v", err)
	}
	if n != 3 {
		t.Fatalf("expected 3 siblings affected, got %d", n)
	}

	if s, _ := jobState(t, st, failedID); s != "failed" {
		t.Errorf("already-failed job should stay failed, got %s", s)
	}
	if s, cr := jobState(t, st, runningID); s != "running" || !cr {
		t.Errorf("running sibling: want running+cancel_requested, got %s cancel=%v", s, cr)
	}
	if s, _ := jobState(t, st, createdID); s != "canceled" {
		t.Errorf("created sibling: want canceled, got %s", s)
	}
	if s, _ := jobState(t, st, pendingID); s != "canceled" {
		t.Errorf("pending sibling: want canceled, got %s", s)
	}

	// Idempotent: a second sweep touches nothing (running already flagged,
	// others already terminal).
	n2, err := st.FailFastCancel(context.Background())
	if err != nil {
		t.Fatalf("FailFastCancel (2nd): %v", err)
	}
	if n2 != 0 {
		t.Errorf("second sweep should be a no-op, affected %d", n2)
	}
}

// allow_failure on the failing job must NOT trigger fail-fast.
func TestFailFastCancel_AllowFailureDoesNotTrigger(t *testing.T) {
	st := newDeployTestStore(t)
	p := makeFFPipeline(t, st, true, twoParallelJobs())
	setJobStatus(t, st, p.ID, "a", "failed", true) // allow_failure
	bID := setJobStatus(t, st, p.ID, "b", "running", false)

	n, err := st.FailFastCancel(context.Background())
	if err != nil {
		t.Fatalf("FailFastCancel: %v", err)
	}
	if n != 0 {
		t.Fatalf("allowed failure must not trigger fail-fast, affected %d", n)
	}
	if s, cr := jobState(t, st, bID); s != "running" || cr {
		t.Errorf("sibling should keep running untouched, got %s cancel=%v", s, cr)
	}
}

// A pipeline without fail_fast is untouched even with a genuine failure.
func TestFailFastCancel_DisabledPipeline(t *testing.T) {
	st := newDeployTestStore(t)
	p := makeFFPipeline(t, st, false, twoParallelJobs())
	setJobStatus(t, st, p.ID, "a", "failed", false)
	bID := setJobStatus(t, st, p.ID, "b", "running", false)

	n, err := st.FailFastCancel(context.Background())
	if err != nil {
		t.Fatalf("FailFastCancel: %v", err)
	}
	if n != 0 {
		t.Fatalf("fail_fast=false must not cancel siblings, affected %d", n)
	}
	if s, cr := jobState(t, st, bID); s != "running" || cr {
		t.Errorf("sibling should keep running untouched, got %s cancel=%v", s, cr)
	}
}

// A non-final (retrying) failure must NOT trigger fail-fast. CompleteJob
// requeues a retryable failure to 'pending' without ever writing status='failed',
// so the sweep sees no genuine failure until retries are exhausted.
func TestFailFastCancel_RetryingDoesNotTriggerEarly(t *testing.T) {
	st := newDeployTestStore(t)
	ctx := context.Background()
	jobs := []compiler.CompiledJob{
		{Name: "flaky", Stage: "build", StageIdx: 0, Script: "false", Env: map[string]string{}, Retry: 1},
		{Name: "sibling", Stage: "build", StageIdx: 0, Script: "sleep 60", Env: map[string]string{}},
	}
	p := makeFFPipeline(t, st, true, jobs)
	flakyID := setJobStatus(t, st, p.ID, "flaky", "running", false)
	sibID := setJobStatus(t, st, p.ID, "sibling", "running", false)

	// First attempt fails with a retry left -> requeued to 'pending', not 'failed'.
	final, _, err := st.CompleteJob(ctx, flakyID, "failed", 1)
	if err != nil {
		t.Fatalf("CompleteJob attempt 1: %v", err)
	}
	if final != "pending" {
		t.Fatalf("expected requeue to pending on first failure, got %q", final)
	}

	n, err := st.FailFastCancel(ctx)
	if err != nil {
		t.Fatalf("FailFastCancel (mid-retry): %v", err)
	}
	if n != 0 {
		t.Fatalf("a non-final failure must not trigger fail-fast, affected %d", n)
	}
	if s, cr := jobState(t, st, sibID); s != "running" || cr {
		t.Errorf("sibling must keep running during retry, got %s cancel=%v", s, cr)
	}

	// Exhaust the retry: pick the requeued job up again and fail it finally.
	rj, err := st.AcquireJob(ctx, proto.AcquireRequest{RunnerID: "r1"})
	if err != nil || rj == nil {
		t.Fatalf("AcquireJob requeued attempt: rj=%v err=%v", rj, err)
	}
	final, _, err = st.CompleteJob(ctx, rj.ID, "failed", 1)
	if err != nil {
		t.Fatalf("CompleteJob attempt 2: %v", err)
	}
	if final != "failed" {
		t.Fatalf("expected final failure on exhausted retry, got %q", final)
	}

	n, err = st.FailFastCancel(ctx)
	if err != nil {
		t.Fatalf("FailFastCancel (retries exhausted): %v", err)
	}
	if n != 1 {
		t.Fatalf("final failure should cancel the 1 running sibling, affected %d", n)
	}
	if s, cr := jobState(t, st, sibID); s != "running" || !cr {
		t.Errorf("sibling should now be flagged for cancel, got %s cancel=%v", s, cr)
	}
}
