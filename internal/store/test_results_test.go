package store

import (
	"context"
	"testing"

	"github.com/priytamjeepandey/forge-ci/internal/compiler"
	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

// newJobIn creates one pipeline with a single job named name for repo, and
// returns that job's id. A fresh pipeline per call keeps each test's rows
// unambiguous without needing to track pipeline ids.
func newJobIn(t *testing.T, st *Store, repo, name string) int64 {
	t.Helper()
	ctx := context.Background()
	jobs := []compiler.CompiledJob{
		{Name: name, Stage: "test", StageIdx: 0, Script: "echo hi", Env: map[string]string{}},
	}
	p, err := st.CreatePipeline(ctx, proto.CreatePipelineRequest{
		Repo: repo, Ref: "main", SHA: "abc", Config: "x",
	}, jobs, nil, false, false)
	if err != nil {
		t.Fatalf("CreatePipeline: %v", err)
	}
	_, pjobs, err := st.GetPipeline(ctx, p.ID)
	if err != nil {
		t.Fatalf("GetPipeline: %v", err)
	}
	return pjobs[0].ID
}

func TestSaveTestCaseResultsReplacesSameJob(t *testing.T) {
	st := newArtifactsTestStore(t)
	ctx := context.Background()
	jobID := newJobIn(t, st, "r/app", "test")

	first := []proto.TestCaseResult{
		{Name: "a", Classname: "pkg", Status: "passed", DurationSeconds: 0.1},
		{Name: "b", Classname: "pkg", Status: "failed", DurationSeconds: 0.2, Message: "boom"},
	}
	if err := st.SaveTestCaseResults(ctx, jobID, first); err != nil {
		t.Fatalf("SaveTestCaseResults: %v", err)
	}

	history, err := st.ListTestCaseHistory(ctx, "r/app", 0)
	if err != nil {
		t.Fatalf("ListTestCaseHistory: %v", err)
	}
	if n := totalRuns(history); n != 2 {
		t.Fatalf("expected 2 recorded runs after first save, got %d: %+v", n, history)
	}

	// A re-upload for the SAME job replaces its rows, not appends to them —
	// matching job_reports' upsert-by-job_id semantics.
	second := []proto.TestCaseResult{
		{Name: "a", Classname: "pkg", Status: "passed", DurationSeconds: 0.15},
	}
	if err := st.SaveTestCaseResults(ctx, jobID, second); err != nil {
		t.Fatalf("SaveTestCaseResults (re-upload): %v", err)
	}
	history, err = st.ListTestCaseHistory(ctx, "r/app", 0)
	if err != nil {
		t.Fatalf("ListTestCaseHistory after re-upload: %v", err)
	}
	if n := totalRuns(history); n != 1 {
		t.Fatalf("expected 1 recorded run after re-upload, got %d: %+v", n, history)
	}
}

func TestListTestCaseHistoryAcrossJobsIsAppendOnly(t *testing.T) {
	st := newArtifactsTestStore(t)
	ctx := context.Background()

	// Three separate jobs (separate pipelines) all reporting the same case: the
	// history across DIFFERENT jobs must accumulate, unlike the same-job replace
	// covered above.
	for _, status := range []string{"failed", "failed", "passed"} {
		jobID := newJobIn(t, st, "r/app", "test")
		err := st.SaveTestCaseResults(ctx, jobID, []proto.TestCaseResult{
			{Name: "flaky", Classname: "pkg", Status: status},
		})
		if err != nil {
			t.Fatalf("SaveTestCaseResults: %v", err)
		}
	}

	history, err := st.ListTestCaseHistory(ctx, "r/app", 0)
	if err != nil {
		t.Fatalf("ListTestCaseHistory: %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("expected 1 distinct case, got %d: %+v", len(history), history)
	}
	runs := history[0].Runs
	if len(runs) != 3 {
		t.Fatalf("expected 3 accumulated runs, got %d: %+v", len(runs), runs)
	}
	// Newest first: the last SaveTestCaseResults call ("passed") must lead.
	if runs[0].Status != "passed" || runs[1].Status != "failed" || runs[2].Status != "failed" {
		t.Fatalf("runs not newest-first: %+v", runs)
	}
}

func TestListTestCaseHistoryLimitIsPerCase(t *testing.T) {
	st := newArtifactsTestStore(t)
	ctx := context.Background()

	// One job reporting two distinct cases, then a second job repeating them —
	// a limit of 1 must keep the latest run of EACH case, not just one row total.
	for i := 0; i < 2; i++ {
		jobID := newJobIn(t, st, "r/app", "test")
		err := st.SaveTestCaseResults(ctx, jobID, []proto.TestCaseResult{
			{Name: "one", Classname: "pkg", Status: "passed"},
			{Name: "two", Classname: "pkg", Status: "passed"},
		})
		if err != nil {
			t.Fatalf("SaveTestCaseResults: %v", err)
		}
	}

	history, err := st.ListTestCaseHistory(ctx, "r/app", 1)
	if err != nil {
		t.Fatalf("ListTestCaseHistory: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("expected 2 distinct cases, got %d: %+v", len(history), history)
	}
	for _, c := range history {
		if len(c.Runs) != 1 {
			t.Fatalf("case %q: expected limit=1 to keep exactly 1 run, got %d", c.Name, len(c.Runs))
		}
	}
}

func TestListTestCaseHistoryEmptyCasesIsNoop(t *testing.T) {
	st := newArtifactsTestStore(t)
	ctx := context.Background()
	jobID := newJobIn(t, st, "r/app", "test")

	if err := st.SaveTestCaseResults(ctx, jobID, nil); err != nil {
		t.Fatalf("SaveTestCaseResults(nil): %v", err)
	}
	history, err := st.ListTestCaseHistory(ctx, "r/app", 0)
	if err != nil {
		t.Fatalf("ListTestCaseHistory: %v", err)
	}
	if len(history) != 0 {
		t.Fatalf("expected no history, got %+v", history)
	}
}

func totalRuns(history []proto.TestCaseHistory) int {
	n := 0
	for _, c := range history {
		n += len(c.Runs)
	}
	return n
}
