package compiler

import "testing"

// TestRulesMergeRequestInclusion proves source routing: a job keyed on
// $CI_PIPELINE_SOURCE == "merge_request" is included for a merge_request
// pipeline and excluded for a push pipeline.
func TestRulesMergeRequestInclusion(t *testing.T) {
	yml := `
stages: [test]
jobs:
  unit:
    stage: test
    script: [make test]
  mr-check:
    stage: test
    script: [echo mr-only]
    rules:
      - if: '$CI_PIPELINE_SOURCE == "merge_request"'
`
	// merge_request source: mr-check included.
	mr, err := Compile(yml, "feature/x", SourceMergeRequest, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := jobNames(mr)["mr-check"]; !ok {
		t.Error("merge_request: mr-check should be included")
	}
	if _, ok := jobNames(mr)["unit"]; !ok {
		t.Error("merge_request: unit should be included")
	}

	// push source: mr-check excluded (unit still runs).
	push, err := Compile(yml, "feature/x", SourcePush, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := jobNames(push)["mr-check"]; ok {
		t.Error("push: mr-check should be excluded")
	}
	if _, ok := jobNames(push)["unit"]; !ok {
		t.Error("push: unit should be included")
	}
}

// TestMergeRequestVarsInContextAndEnv proves the CI_MERGE_REQUEST_* extra context
// is (a) visible to rules if: expressions and (b) injected into every emitted
// job's Env so job scripts can read it.
func TestMergeRequestVarsInContextAndEnv(t *testing.T) {
	extra := map[string]string{
		"CI_PIPELINE_SOURCE":             SourceMergeRequest,
		"CI_MERGE_REQUEST_IID":           "42",
		"CI_MERGE_REQUEST_SOURCE_BRANCH": "feature/x",
		"CI_MERGE_REQUEST_TARGET_BRANCH": "main",
		"CI_MERGE_REQUEST_TITLE":         "Add widget",
	}
	yml := `
stages: [test]
jobs:
  to-main:
    stage: test
    script: [echo hi]
    rules:
      - if: '$CI_MERGE_REQUEST_TARGET_BRANCH == "main"'
`
	// (a) rules see the MR var: the target-branch rule includes the job.
	jobs, err := Compile(yml, "feature/x", SourceMergeRequest, nil, extra)
	if err != nil {
		t.Fatal(err)
	}
	j, ok := jobNames(jobs)["to-main"]
	if !ok {
		t.Fatal("to-main should be included (target branch is main)")
	}
	// (b) every MR var is present in the job Env for scripts.
	for k, want := range extra {
		if got := j.Env[k]; got != want {
			t.Errorf("job Env[%q] = %q, want %q", k, got, want)
		}
	}

	// A different target branch excludes the job — confirms the rule really reads
	// the extra context and is not always-true.
	extra2 := map[string]string{"CI_MERGE_REQUEST_TARGET_BRANCH": "develop"}
	if _, err := Compile(yml, "feature/x", SourceMergeRequest, nil, extra2); err == nil {
		t.Error("target=develop: expected no-jobs-match error")
	}
}

// TestMergeRequestExtraDoesNotLeakToPush confirms existing (non-MR) callers that
// pass no extra map get no CI_MERGE_REQUEST_* vars in job env.
func TestMergeRequestExtraDoesNotLeakToPush(t *testing.T) {
	yml := `
stages: [build]
jobs:
  build:
    stage: build
    script: [echo hi]
`
	jobs, err := Compile(yml, "main", SourcePush, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := jobNames(jobs)["build"].Env["CI_MERGE_REQUEST_IID"]; ok {
		t.Error("push pipeline should not carry CI_MERGE_REQUEST_* env")
	}
}
