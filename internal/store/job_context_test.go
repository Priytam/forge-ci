package store

import (
	"context"
	"testing"

	"github.com/priytamjeepandey/forge-ci/internal/compiler"
	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

func TestPredefinedURLAndRepoVars(t *testing.T) {
	t.Setenv("EXTERNAL_URL", "https://forge.example.com/")
	t.Setenv("FRONTEND_URL", "")
	v := PredefinedVars(JobContext{
		Repo: "acme/api", Ref: "main", SHA: "abc", PipelineID: 7, JobID: 42,
		DefaultBranch: "trunk", CloneURL: "https://bot:s3cret@git.example.com/acme/api.git",
	})
	for k, want := range map[string]string{
		"CI_SERVER_URL":     "https://forge.example.com",
		"CI_API_V1_URL":     "https://forge.example.com/api/v1",
		"CI_PIPELINE_URL":   "https://forge.example.com/pipelines/7",
		"CI_JOB_URL":        "https://forge.example.com/jobs/42",
		"CI_DEFAULT_BRANCH": "trunk",
		"CI_REPOSITORY_URL": "https://git.example.com/acme/api.git",
	} {
		if v[k] != want {
			t.Errorf("%s = %q, want %q", k, v[k], want)
		}
	}
	t.Setenv("EXTERNAL_URL", "")
	if _, ok := PredefinedVars(JobContext{Repo: "x"})["CI_PIPELINE_URL"]; ok {
		t.Error("URL vars must be unset when no server URL is configured")
	}
}

// TestTagPipelineHasNoBranch: a tag push (CI_COMMIT_TAG passed by the
// webhook) reaches the job with the tag and without CI_COMMIT_BRANCH.
func TestTagPipelineHasNoBranch(t *testing.T) {
	st := newArtifactsTestStore(t)
	ctx := context.Background()
	yml := "stages: [b]\njobs:\n  release:\n    stage: b\n    rules:\n      - if: $CI_COMMIT_TAG\n    script: [x]\n"
	tag := map[string]string{"CI_COMMIT_TAG": "v1.2.0"}
	jobs, err := compiler.CompileWithChanges(yml, "v1.2.0", compiler.SourcePush, nil, nil, tag)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("compile = %v %v", jobs, err)
	}
	if none, _ := compiler.Compile(yml, "main", compiler.SourcePush, nil); len(none) != 0 {
		t.Error("a branch push must not match if: $CI_COMMIT_TAG")
	}
	if _, err := st.CreatePipeline(ctx, proto.CreatePipelineRequest{Repo: "acme/api", Ref: "v1.2.0", SHA: "abc", Config: yml}, jobs, nil, false, false); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PromoteReadyJobs(ctx); err != nil {
		t.Fatal(err)
	}
	rj, err := st.AcquireJob(ctx, proto.AcquireRequest{RunnerID: "r1"})
	if err != nil || rj == nil {
		t.Fatalf("acquire: %v %v", rj, err)
	}
	if rj.Env["CI_COMMIT_TAG"] != "v1.2.0" || rj.Env["CI_COMMIT_REF_NAME"] != "v1.2.0" {
		t.Errorf("tag env = %v", rj.Env)
	}
	if _, ok := rj.Env["CI_COMMIT_BRANCH"]; ok {
		t.Error("tag pipeline must not set CI_COMMIT_BRANCH")
	}
}

func TestRetryWithDebugTrace(t *testing.T) {
	st := newArtifactsTestStore(t)
	ctx := context.Background()
	p := createPipelineT(t, st, "acme/cos", "main", retryDAG)
	jobs, _ := liveJobs(t, st, p.ID)
	setStatus(t, st, jobs["build"].ID, "success")
	setStatus(t, st, jobs["lint"].ID, "success")
	setStatus(t, st, jobs["unit"].ID, "failed")
	setStatus(t, st, jobs["deploy"].ID, "canceled")
	res, err := st.RetryJobWith(ctx, jobs["unit"].ID, RetryOpts{Debug: true})
	if err != nil {
		t.Fatal(err)
	}
	env := func(id int64) string {
		var v string
		_ = st.pool.QueryRow(ctx, `SELECT COALESCE(env->>'CI_DEBUG_TRACE','') FROM jobs WHERE id=$1`, id).Scan(&v)
		return v
	}
	if env(res.Retried[jobs["unit"].ID]) != "true" {
		t.Error("the retried job must run with CI_DEBUG_TRACE=true")
	}
	if env(res.Retried[jobs["deploy"].ID]) != "" {
		t.Error("dependents re-run normally, without debug trace")
	}
}
