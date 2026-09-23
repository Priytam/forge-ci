package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/priytamjeepandey/forge-ci/internal/compiler"
	"github.com/priytamjeepandey/forge-ci/internal/proto"
)

// retryYAML has a job name distinct from anything a "current registered
// config" test fixture would use, so a retry that accidentally re-resolved
// instead of replaying the stored config_yaml would be caught by name, not
// just by accident matching.
const retryYAML = `
stages: [test]
jobs:
  retry_marker_job:
    stage: test
    script: [echo retried]
`

// TestRetryPipelineReplaysStoredConfig proves a retry reproduces the ORIGINAL
// pipeline's config_yaml verbatim — not a fresh resolve, which could silently
// pick up a registry change made after the original run. It also checks the
// new pipeline is a distinct row (jobs are immutable history, never
// resurrected) with the same repo/ref/sha.
func TestRetryPipelineReplaysStoredConfig(t *testing.T) {
	st, _ := newCFRStore(t, "")
	server := New(st, nil, nil)
	ctx := context.Background()

	const repo = "acme/retry-target"
	jobs, err := compiler.Compile(retryYAML, "main", compiler.SourceAPI, st.TemplateResolver(ctx, repo))
	if err != nil {
		t.Fatalf("compile seed config: %v", err)
	}
	opts, err := compiler.Options(retryYAML)
	if err != nil {
		t.Fatalf("compiler.Options: %v", err)
	}
	original, err := st.CreatePipeline(ctx, proto.CreatePipelineRequest{
		Repo: repo, Ref: "main", SHA: "deadbeef0001", Config: retryYAML,
		TriggeredBy:   "alice",
		CommitAuthor:  "dana",
		CommitMessage: "the commit being retried",
	}, jobs, nil, opts.AutoCancel, opts.FailFast)
	if err != nil {
		t.Fatalf("seed CreatePipeline: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/pipelines/"+strconv.FormatInt(original.ID, 10)+"/retry", nil)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("retry status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Pipeline proto.Pipeline `json:"pipeline"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	retried := resp.Pipeline

	if retried.ID == original.ID {
		t.Fatal("retry returned the SAME pipeline id — jobs must be immutable history, retry must create a new row")
	}
	if retried.Repo != repo || retried.Ref != "main" || retried.SHA != "deadbeef0001" {
		t.Errorf("retry pipeline repo/ref/sha = %s/%s/%s, want %s/main/deadbeef0001",
			retried.Repo, retried.Ref, retried.SHA, repo)
	}
	if retried.CommitAuthor != "dana" || retried.CommitMessage != "the commit being retried" {
		t.Errorf("retry did not carry commit identity forward: author=%q message=%q",
			retried.CommitAuthor, retried.CommitMessage)
	}

	_, newJobs, err := st.GetPipeline(ctx, retried.ID)
	if err != nil {
		t.Fatalf("GetPipeline(retried): %v", err)
	}
	if len(newJobs) != 1 || newJobs[0].Name != "retry_marker_job" {
		t.Fatalf("retried pipeline jobs = %v, want exactly [retry_marker_job] — proves it replayed the STORED config, not a re-resolve", newJobs)
	}
}

// TestRetryPipelineNotFound checks the 404 path is a clean error, not a panic
// on a nil RetrySource.
func TestRetryPipelineNotFound(t *testing.T) {
	st, _ := newCFRStore(t, "")
	server := New(st, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/pipelines/999999/retry", nil)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}
