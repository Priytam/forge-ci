package store

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

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

	repos, total, err := st.ListRepos(ctx, RepoQuery{})
	if err != nil {
		t.Fatalf("ListRepos: %v", err)
	}
	if total != 4 {
		t.Errorf("total = %d, want 4", total)
	}
	var names []string
	byRepo := map[string]proto.RepoSummary{}
	for _, r := range repos {
		names = append(names, r.Repo)
		byRepo[r.Repo] = r
	}
	// acme/never was registered after every run, so it is the most recently
	// active; acme/old's registration doesn't count as activity (it has runs).
	if want := []string{"acme/never", "acme/mid", "acme/busy", "acme/old"}; !reflect.DeepEqual(names, want) {
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
	if r := byRepo["acme/busy"]; len(r.Refs) != 7 || r.RefCount != 7 {
		t.Errorf("acme/busy refs = %d (count %d), want 7", len(r.Refs), r.RefCount)
	}

	mid := byRepo["acme/mid"]
	if mid.LastPipeline == nil || mid.LastPipeline.ID != lastMid || mid.LastPipeline.Status != "created" {
		t.Fatalf("acme/mid last pipeline = %+v, want id %d created", mid.LastPipeline, lastMid)
	}
	if mid.RefCount != 3 {
		t.Errorf("acme/mid ref_count = %d, want 3", mid.RefCount)
	}
	if !mid.LastActivityAt.Equal(mid.LastPipeline.CreatedAt) {
		t.Errorf("last activity %v != last pipeline created %v", mid.LastActivityAt, mid.LastPipeline.CreatedAt)
	}
	if byRepo["acme/never"].LastPipeline != nil || byRepo["acme/never"].LastActivityAt.IsZero() {
		t.Errorf("acme/never = %+v, want no pipeline and a registration time", byRepo["acme/never"])
	}
}

func TestListReposSearchSortPage(t *testing.T) {
	st := newDeployTestStore(t)
	ctx := context.Background()
	for _, repo := range []string{"acme/web", "Acme/API", "tools/cli", "acme/worker"} {
		if _, err := st.CreatePipeline(ctx, proto.CreatePipelineRequest{
			Repo: repo, Ref: "main", SHA: "abc", Config: "x",
		}, nil, nil, false, false); err != nil {
			t.Fatalf("CreatePipeline: %v", err)
		}
	}
	names := func(q RepoQuery, wantTotal int) []string {
		t.Helper()
		repos, total, err := st.ListRepos(ctx, q)
		if err != nil {
			t.Fatalf("ListRepos(%+v): %v", q, err)
		}
		if total != wantTotal {
			t.Errorf("ListRepos(%+v) total = %d, want %d", q, total, wantTotal)
		}
		out := []string{}
		for _, r := range repos {
			out = append(out, r.Repo)
		}
		return out
	}
	eq := func(got, want []string) {
		t.Helper()
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	}
	eq(names(RepoQuery{}, 4), []string{"acme/worker", "tools/cli", "Acme/API", "acme/web"})
	eq(names(RepoQuery{Sort: "name"}, 4), []string{"Acme/API", "acme/web", "acme/worker", "tools/cli"})
	eq(names(RepoQuery{Sort: "name", Limit: 2, Offset: 1}, 4), []string{"acme/web", "acme/worker"})
	eq(names(RepoQuery{Search: "ACME/", Sort: "name"}, 3), []string{"Acme/API", "acme/web", "acme/worker"})
	eq(names(RepoQuery{Search: "w", Limit: 1}, 2), []string{"acme/worker"})
	eq(names(RepoQuery{Search: "%"}, 0), []string{})
	eq(names(RepoQuery{Search: "acme_"}, 0), []string{})
	eq(names(RepoQuery{Offset: 10, Limit: 5}, 4), []string{})
}

// TestStoredPipelineStatusMatchesDerive checks that the jobs trigger keeps
// pipelines.status equal to deriveStatus for every mix of job statuses.
func TestStoredPipelineStatusMatchesDerive(t *testing.T) {
	st := newDeployTestStore(t)
	ctx := context.Background()
	jobStatuses := []string{"created", "pending", "blocked", "running", "success", "failed", "canceled"}
	jobs := []compiler.CompiledJob{
		{Name: "a", Stage: "s", Script: "true", Env: map[string]string{}},
		{Name: "b", Stage: "s", Script: "true", Env: map[string]string{}},
	}
	p, err := st.CreatePipeline(ctx, proto.CreatePipelineRequest{
		Repo: "acme/x", Ref: "main", SHA: "abc", Config: "x",
	}, jobs, nil, false, false)
	if err != nil {
		t.Fatalf("CreatePipeline: %v", err)
	}
	stored := func() string {
		t.Helper()
		var s string
		if err := st.pool.QueryRow(ctx, `SELECT status FROM pipelines WHERE id=$1`, p.ID).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	for _, a := range jobStatuses {
		for _, b := range jobStatuses {
			for _, allowB := range []bool{false, true} {
				if _, err := st.pool.Exec(ctx,
					`UPDATE jobs SET status = CASE name WHEN 'a' THEN $2 ELSE $3 END,
					                 allow_failure = (name = 'b' AND $4)
					 WHERE pipeline_id=$1`, p.ID, a, b, allowB); err != nil {
					t.Fatal(err)
				}
				bEff := b
				if b == "failed" && allowB {
					bEff = "success"
				}
				if got, want := stored(), deriveStatus([]string{a, bEff}); got != want {
					t.Errorf("jobs %s,%s (allow_failure b=%v): stored %q, derived %q", a, b, allowB, got, want)
				}
			}
		}
	}
	if _, err := st.pool.Exec(ctx, `DELETE FROM jobs WHERE pipeline_id=$1`, p.ID); err != nil {
		t.Fatal(err)
	}
	if got := stored(); got != "created" {
		t.Errorf("after deleting jobs: stored %q, want created", got)
	}
}

// TestStoredPipelineStatusConcurrentJobs finishes the last two jobs of a
// pipeline in overlapping transactions; the pipeline must end up success, not
// stuck on the status either transaction saw alone.
func TestStoredPipelineStatusConcurrentJobs(t *testing.T) {
	st := newDeployTestStore(t)
	ctx := context.Background()
	p, err := st.CreatePipeline(ctx, proto.CreatePipelineRequest{
		Repo: "acme/x", Ref: "main", SHA: "abc", Config: "x",
	}, []compiler.CompiledJob{
		{Name: "a", Stage: "s", Script: "true", Env: map[string]string{}},
		{Name: "b", Stage: "s", Script: "true", Env: map[string]string{}},
	}, nil, false, false)
	if err != nil {
		t.Fatalf("CreatePipeline: %v", err)
	}
	if _, err := st.pool.Exec(ctx, `UPDATE jobs SET status='running' WHERE pipeline_id=$1`, p.ID); err != nil {
		t.Fatal(err)
	}
	tx1, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx1.Exec(ctx, `UPDATE jobs SET status='success' WHERE pipeline_id=$1 AND name='a'`, p.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		// Blocks on the pipeline row tx1 holds, then must see tx1's job.
		_, err := st.pool.Exec(ctx, `UPDATE jobs SET status='success' WHERE pipeline_id=$1 AND name='b'`, p.ID)
		done <- err
	}()
	time.Sleep(200 * time.Millisecond)
	if err := tx1.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var got string
	if err := st.pool.QueryRow(ctx, `SELECT status FROM pipelines WHERE id=$1`, p.ID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "success" {
		t.Errorf("stored status = %q, want success", got)
	}
}
