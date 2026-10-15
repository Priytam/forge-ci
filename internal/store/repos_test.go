package store

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/priytamjeepandey/forge-ci/internal/compiler"
	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

// TestListReposCountsEveryPipeline seeds more pipelines than the old 200-row
// window and checks that every repo gets a card whose counts, recent statuses
// and refs cover all of its pipelines.
func TestListReposCountsEveryPipeline(t *testing.T) {
	st := newDeployTestStore(t)
	ctx := context.Background()

	// mk creates a pipeline with one job (none when status is "") and forces
	// the job into status. allowFailure marks a failed job as allowed to fail.
	mk := func(repo, ref, status string, allowFailure bool) int64 {
		t.Helper()
		var jobs []compiler.CompiledJob
		if status != "" {
			jobs = []compiler.CompiledJob{{
				Name: "build", Stage: "build", Script: "true", Env: map[string]string{},
			}}
		}
		p, err := st.CreatePipeline(ctx, proto.CreatePipelineRequest{
			Repo: repo, Ref: ref, SHA: "abc123", Config: "x", TriggeredBy: "dev",
		}, jobs, nil, false, false)
		if err != nil {
			t.Fatalf("CreatePipeline: %v", err)
		}
		if status != "" {
			if _, err := st.pool.Exec(ctx,
				`UPDATE jobs SET status=$2, allow_failure=$3 WHERE pipeline_id=$1`,
				p.ID, status, allowFailure); err != nil {
				t.Fatalf("set job status: %v", err)
			}
		}
		return p.ID
	}

	// acme/old ran long ago: 210+ newer pipelines push it out of any
	// newest-200 window.
	mk("acme/old", "main", "success", false)
	mk("acme/old", "release", "failed", false)
	mk("acme/old", "main", "success", false)

	busyStatus := []string{"success", "failed", "canceled", "running"}
	busyOK, busyBad := 0, 0
	for i := 0; i < 210; i++ {
		s := busyStatus[i%len(busyStatus)]
		mk("acme/busy", fmt.Sprintf("feature-%d", i%7), s, false)
		switch s {
		case "success":
			busyOK++
		case "failed":
			busyBad++
		}
	}

	// acme/mid is newest. Its recent statuses (newest first) cover a
	// job-less pipeline (created) and an allowed failure (success).
	mk("acme/mid", "main", "failed", false)
	mk("acme/mid", "main", "success", false)
	mk("acme/mid", "dev", "failed", true)
	mk("acme/mid", "main", "blocked", false)
	mk("acme/mid", "dev", "pending", false)
	lastMid := mk("acme/mid", "topic", "", false)

	if err := st.RegisterRepo(ctx, proto.RepoRegistration{
		Repo: "acme/never", Provider: "other", CloneURL: "https://example.com/never.git",
	}); err != nil {
		t.Fatalf("RegisterRepo: %v", err)
	}
	// A registered repo that has run must not get a second card.
	if err := st.RegisterRepo(ctx, proto.RepoRegistration{
		Repo: "acme/old", Provider: "other", CloneURL: "https://example.com/old.git",
	}); err != nil {
		t.Fatalf("RegisterRepo: %v", err)
	}

	repos, err := st.ListRepos(ctx)
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	var names []string
	byRepo := map[string]proto.RepoSummary{}
	for _, r := range repos {
		names = append(names, r.Repo)
		byRepo[r.Repo] = r
	}
	if want := []string{"acme/mid", "acme/busy", "acme/old", "acme/never"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("repo order = %v, want %v", names, want)
	}

	check := func(repo string, n, ok, bad int, recent, refs []string) {
		t.Helper()
		r := byRepo[repo]
		if r.PipelineCount != n || r.SuccessCount != ok || r.FailedCount != bad {
			t.Errorf("%s counts = %d/%d/%d, want %d/%d/%d", repo,
				r.PipelineCount, r.SuccessCount, r.FailedCount, n, ok, bad)
		}
		if !reflect.DeepEqual(r.RecentStatuses, recent) {
			t.Errorf("%s recent = %v, want %v", repo, r.RecentStatuses, recent)
		}
		if refs != nil && !reflect.DeepEqual(r.Refs, refs) {
			t.Errorf("%s refs = %v, want %v", repo, r.Refs, refs)
		}
	}
	check("acme/old", 3, 2, 1, []string{"success", "failed", "success"}, []string{"main", "release"})
	check("acme/busy", 210, busyOK, busyBad,
		[]string{"failed", "success", "running", "canceled", "failed"}, nil)
	check("acme/mid", 6, 2, 1,
		[]string{"created", "running", "blocked", "success", "success"}, []string{"topic", "dev", "main"})
	check("acme/never", 0, 0, 0, []string{}, []string{})
	if got := len(byRepo["acme/busy"].Refs); got != 7 {
		t.Errorf("acme/busy refs = %d, want 7", got)
	}

	mid := byRepo["acme/mid"]
	if mid.LastPipeline == nil || mid.LastPipeline.ID != lastMid || mid.LastPipeline.Status != "created" {
		t.Fatalf("acme/mid last pipeline = %+v, want id %d created", mid.LastPipeline, lastMid)
	}
	if !mid.LastActivityAt.Equal(mid.LastPipeline.CreatedAt) {
		t.Errorf("last activity %v != last pipeline created %v", mid.LastActivityAt, mid.LastPipeline.CreatedAt)
	}
	if byRepo["acme/never"].LastPipeline != nil || byRepo["acme/never"].LastActivityAt.IsZero() {
		t.Errorf("acme/never = %+v, want no pipeline and a registration time", byRepo["acme/never"])
	}
}
