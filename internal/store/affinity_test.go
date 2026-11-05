package store

import (
	"context"
	"testing"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

const affinityYAML = `
stages: [build]
jobs:
  image:
    stage: build
    runner_affinity: true
    script: [docker build .]
`

// TestRunnerAffinity: after r1 builds the job successfully, the next run
// waits for r1 (r2 gets nothing) while r1 is online and the job is fresh in
// the queue; with r1 offline, or after the wait window, r2 takes it.
func TestRunnerAffinity(t *testing.T) {
	st := newArtifactsTestStore(t)
	ctx := context.Background()
	acquire := func(runner string) *proto.RunnerJob {
		t.Helper()
		rj, err := st.AcquireJob(ctx, proto.AcquireRequest{RunnerID: runner})
		if err != nil {
			t.Fatal(err)
		}
		return rj
	}
	// First run: no affinity yet, anyone may take it; r1 does and succeeds.
	createPipelineT(t, st, "acme/cos", "main", affinityYAML)
	if _, err := st.PromoteReadyJobs(ctx); err != nil {
		t.Fatal(err)
	}
	acquire("r2") // registers r2 (it may take the first job — fine)
	if _, err := st.pool.Exec(ctx, `UPDATE jobs SET status='pending', runner_id=NULL WHERE status='running'`); err != nil {
		t.Fatal(err)
	}
	first := acquire("r1")
	if first == nil {
		t.Fatal("r1 got nothing on the first run")
	}
	if _, _, err := st.CompleteJob(ctx, first.ID, "success", 0); err != nil {
		t.Fatal(err)
	}
	var pref string
	_ = st.pool.QueryRow(ctx, `SELECT runner_id FROM runner_affinity WHERE key='acme/cos#image'`).Scan(&pref)
	if pref != "r1" {
		t.Fatalf("affinity = %q, want r1", pref)
	}

	// Second run: r2 must leave it for r1.
	createPipelineT(t, st, "acme/cos", "main", affinityYAML)
	if _, err := st.PromoteReadyJobs(ctx); err != nil {
		t.Fatal(err)
	}
	if got := acquire("r2"); got != nil {
		t.Fatalf("r2 took a job preferring r1 within the window: %+v", got)
	}
	if got := acquire("r1"); got == nil {
		t.Fatal("r1 did not get its preferred job")
	} else if _, _, err := st.CompleteJob(ctx, got.ID, "success", 0); err != nil {
		t.Fatal(err)
	}

	// Third run with r1 offline: r2 takes it straight away.
	createPipelineT(t, st, "acme/cos", "main", affinityYAML)
	if _, err := st.PromoteReadyJobs(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx, `UPDATE runners SET last_contact_at = now() - interval '5 minutes' WHERE id='r1'`); err != nil {
		t.Fatal(err)
	}
	if got := acquire("r2"); got == nil {
		t.Fatal("r2 should take the job when r1 is offline")
	} else if _, _, err := st.CompleteJob(ctx, got.ID, "success", 0); err != nil {
		t.Fatal(err)
	}
	// Affinity moves to whoever succeeded last (r2).
	_ = st.pool.QueryRow(ctx, `SELECT runner_id FROM runner_affinity WHERE key='acme/cos#image'`).Scan(&pref)
	if pref != "r2" {
		t.Errorf("affinity after r2's run = %q, want r2", pref)
	}

	// Fourth run prefers r2; r1 back online can still take it once it waited
	// past the window.
	createPipelineT(t, st, "acme/cos", "main", affinityYAML)
	if _, err := st.PromoteReadyJobs(ctx); err != nil {
		t.Fatal(err)
	}
	if got := acquire("r1"); got != nil {
		t.Fatalf("r1 took r2's job inside the window: %+v", got)
	}
	if _, err := st.pool.Exec(ctx, `UPDATE jobs SET queued_at = now() - interval '2 minutes' WHERE status='pending'`); err != nil {
		t.Fatal(err)
	}
	if got := acquire("r1"); got == nil {
		t.Fatal("after the affinity window any runner may take the job")
	}
}

// TestNoAffinityByDefault: jobs without runner_affinity are never held.
func TestNoAffinityByDefault(t *testing.T) {
	st := newArtifactsTestStore(t)
	ctx := context.Background()
	yml := "stages: [b]\njobs:\n  build:\n    stage: b\n    script: [make]\n"
	createPipelineT(t, st, "acme/cos", "main", yml)
	if _, err := st.PromoteReadyJobs(ctx); err != nil {
		t.Fatal(err)
	}
	rj, err := st.AcquireJob(ctx, proto.AcquireRequest{RunnerID: "r1"})
	if err != nil || rj == nil {
		t.Fatalf("acquire: %v %v", rj, err)
	}
	if _, _, err := st.CompleteJob(ctx, rj.ID, "success", 0); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = st.pool.QueryRow(ctx, `SELECT count(*) FROM runner_affinity`).Scan(&n)
	if n != 0 {
		t.Errorf("affinity recorded for a job that did not opt in")
	}
}
