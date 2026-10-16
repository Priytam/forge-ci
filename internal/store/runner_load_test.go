package store

import (
	"context"
	"testing"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

func TestRunnerLoadQueuesByTags(t *testing.T) {
	st := newArtifactsTestStore(t)
	ctx := context.Background()
	linux := "stages: [b]\njobs:\n  build:\n    stage: b\n    tags: [linux]\n    script: [make]\n"
	gpu := "stages: [b]\njobs:\n  train:\n    stage: b\n    tags: [gpu, linux]\n    script: [train]\n"
	for i := 0; i < 3; i++ {
		createPipelineT(t, st, "acme/cos", "main", linux)
	}
	createPipelineT(t, st, "acme/ml", "main", gpu)
	if _, err := st.PromoteReadyJobs(ctx); err != nil {
		t.Fatal(err)
	}
	// One linux runner online picks up one job.
	if _, err := st.AcquireJob(ctx, proto.AcquireRequest{RunnerID: "lin-1", Tags: []string{"linux"}}); err != nil {
		t.Fatal(err)
	}
	load, err := st.GetRunnerLoad(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if load.Pending != 3 || load.Running != 1 {
		t.Fatalf("pending/running = %d/%d, want 3/1", load.Pending, load.Running)
	}
	byTags := map[string]QueueStat{}
	for _, q := range load.Queues {
		byTags[tagsKey(q.Tags)] = q
	}
	if q := byTags["linux"]; q.Pending != 2 || q.EligibleOnline != 1 || q.EligibleBusy != 1 {
		t.Errorf("linux queue = %+v", q)
	}
	if q := byTags["gpu,linux"]; q.Pending != 1 || q.EligibleOnline != 0 {
		t.Errorf("gpu queue = %+v (no gpu runner online)", q)
	}
	if len(load.Runners) != 1 || load.Runners[0].Running != 1 || len(load.Runners[0].RunningJobs) != 1 {
		t.Errorf("runners = %+v", load.Runners)
	}
	var queued int
	_ = st.pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE status='pending' AND queued_at IS NOT NULL`).Scan(&queued)
	if queued != 3 {
		t.Errorf("pending jobs with queued_at = %d, want 3", queued)
	}
}
