package store

import (
	"context"
	"testing"

	"github.com/priytamjeepandey/forge-ci/internal/compiler"
	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

func TestPredefinedVarsBranchAndTag(t *testing.T) {
	branch := PredefinedVars(JobContext{
		Repo: "acme/cos", Ref: "refs/heads/feature/Login_Fix", SHA: "0123456789abcdef",
		Source: "push", PipelineID: 7, JobID: 42, JobName: "build", CommitMessage: "Fix login\n\nbody",
	})
	want := map[string]string{
		"CI_COMMIT_SHA":        "0123456789abcdef",
		"CI_COMMIT_SHORT_SHA":  "01234567",
		"CI_COMMIT_REF_NAME":   "feature/Login_Fix",
		"CI_COMMIT_BRANCH":     "feature/Login_Fix",
		"CI_COMMIT_REF_SLUG":   "feature-login-fix",
		"CI_PIPELINE_ID":       "7",
		"CI_JOB_ID":            "42",
		"CI_PROJECT_PATH":      "acme/cos",
		"CI_PROJECT_NAME":      "cos",
		"CI_PROJECT_NAMESPACE": "acme",
		"CI_COMMIT_TITLE":      "Fix login",
		"CI_PIPELINE_SOURCE":   "push",
	}
	for k, v := range want {
		if branch[k] != v {
			t.Errorf("%s = %q, want %q", k, branch[k], v)
		}
	}
	if _, ok := branch["CI_COMMIT_TAG"]; ok {
		t.Error("a branch pipeline must not set CI_COMMIT_TAG")
	}

	tag := PredefinedVars(JobContext{Repo: "ecom", Ref: "refs/tags/v1.2.0", SHA: "abc"})
	if tag["CI_COMMIT_TAG"] != "v1.2.0" || tag["CI_COMMIT_REF_NAME"] != "v1.2.0" {
		t.Errorf("tag vars = %v", tag)
	}
	if _, ok := tag["CI_COMMIT_BRANCH"]; ok {
		t.Error("a tag pipeline must not set CI_COMMIT_BRANCH")
	}
	if tag["CI_PROJECT_NAME"] != "ecom" || tag["CI_COMMIT_SHORT_SHA"] != "abc" {
		t.Errorf("short repo/sha handling = %v", tag)
	}
}

// TestAcquireJobInjectsCommitVars: a runner picking up a job gets the commit
// context in its environment, and the job's own variables still win.
func TestAcquireJobInjectsCommitVars(t *testing.T) {
	st := newArtifactsTestStore(t)
	ctx := context.Background()
	yml := `
stages: [build]
jobs:
  build:
    stage: build
    environment: staging
    variables:
      CI_PROJECT_NAME: overridden
    script: [env]
`
	jobs, err := compiler.Compile(yml, "main", compiler.SourceAPI, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreatePipeline(ctx, proto.CreatePipelineRequest{
		Repo: "acme/cos", Ref: "main", SHA: "feedfacecafebeef", Config: yml,
		TriggeredBy: "harish@acme.io", CommitMessage: "Ship it", Source: "push",
	}, jobs, nil, false, false); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PromoteReadyJobs(ctx); err != nil {
		t.Fatal(err)
	}
	rj, err := st.AcquireJob(ctx, proto.AcquireRequest{RunnerID: "r1"})
	if err != nil || rj == nil {
		t.Fatalf("AcquireJob: %v %v", rj, err)
	}
	for k, v := range map[string]string{
		"CI_COMMIT_SHA":       "feedfacecafebeef",
		"CI_COMMIT_BRANCH":    "main",
		"CI_COMMIT_REF_NAME":  "main",
		"CI_PIPELINE_SOURCE":  "push",
		"CI_ENVIRONMENT_NAME": "staging",
		"CI_COMMIT_TITLE":     "Ship it",
		"CI_TRIGGERED_BY":     "harish@acme.io",
		"CI_PROJECT_NAME":     "overridden",
	} {
		if rj.Env[k] != v {
			t.Errorf("job env %s = %q, want %q", k, rj.Env[k], v)
		}
	}
}
