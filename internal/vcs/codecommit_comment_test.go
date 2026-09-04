package vcs

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Every phase must render a comment that names the outcome and links the
// pipeline. There is no status API to fake, so the text IS the report.
func TestMapCodeCommitComment(t *testing.T) {
	tests := []struct {
		status   string
		wantWord string
	}{
		{"success", "succeeded"},
		{"failed", "failed"},
		{"canceled", "canceled"},
		{"blocked", "approval"},
		{"pending", "queued"},
		{"running", "running"},
		{"something-new", "running"},
	}
	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			got := MapCodeCommitComment(tt.status, "https://forge.example.com/pipelines/7")
			if !strings.Contains(strings.ToLower(got), tt.wantWord) {
				t.Errorf("comment for %q = %q, want it to mention %q", tt.status, got, tt.wantWord)
			}
			if !strings.Contains(got, StatusContext) {
				t.Errorf("comment %q does not identify itself as %s", got, StatusContext)
			}
			if !strings.Contains(got, "https://forge.example.com/pipelines/7") {
				t.Errorf("comment %q does not link the pipeline", got)
			}
		})
	}
}

func TestMapCodeCommitCommentWithoutURL(t *testing.T) {
	got := MapCodeCommitComment("success", "")
	if strings.Contains(got, "\n\n") {
		t.Errorf("comment %q has a dangling link section", got)
	}
}

// The comment must be anchored to the PR and to both commits, or CodeCommit
// rejects it.
func TestPostCodeCommitComment(t *testing.T) {
	var gotTarget string
	var gotIn map[string]any
	ccStub(t, func(target string, in map[string]any) (int, string) {
		gotTarget, gotIn = target, in
		return http.StatusOK, `{"comment":{"commentId":"c1"}}`
	})

	p := NewPoster()
	permanent, err := p.PostCodeCommitComment(context.Background(),
		CodeCommitRepo{Region: "ap-south-1", Name: "tablespace-api"}, "",
		"42", "2222", "1111", "**forge-ci** — ✅ Pipeline succeeded")
	if err != nil {
		t.Fatalf("PostCodeCommitComment: %v (permanent=%v)", err, permanent)
	}
	if !strings.HasSuffix(gotTarget, "PostCommentForPullRequest") {
		t.Errorf("X-Amz-Target = %q", gotTarget)
	}
	if gotIn["pullRequestId"] != "42" {
		t.Errorf("pullRequestId = %v", gotIn["pullRequestId"])
	}
	if gotIn["repositoryName"] != "tablespace-api" {
		t.Errorf("repositoryName = %v", gotIn["repositoryName"])
	}
	if gotIn["beforeCommitId"] != "2222" {
		t.Errorf("beforeCommitId = %v, want the PR destination commit", gotIn["beforeCommitId"])
	}
	if gotIn["afterCommitId"] != "1111" {
		t.Errorf("afterCommitId = %v, want the pipeline sha", gotIn["afterCommitId"])
	}
	if !strings.Contains(gotIn["content"].(string), "succeeded") {
		t.Errorf("content = %v", gotIn["content"])
	}
}

// beforeCommitId is REQUIRED by the API: omitting it fails client-side
// validation, which the scheduler would misread as transient and retry forever.
// When no destination commit is known the call must still go out, anchored at
// the after commit.
func TestPostCodeCommitCommentWithoutBaseSHA(t *testing.T) {
	var gotIn map[string]any
	ccStub(t, func(_ string, in map[string]any) (int, string) {
		gotIn = in
		return http.StatusOK, `{"comment":{"commentId":"c1"}}`
	})
	p := NewPoster()
	if _, err := p.PostCodeCommitComment(context.Background(),
		CodeCommitRepo{Region: "ap-south-1", Name: "r"}, "", "1", "", "abc", "body"); err != nil {
		t.Fatalf("a missing base commit must not block the comment: %v", err)
	}
	if gotIn["beforeCommitId"] != "abc" {
		t.Errorf("beforeCommitId = %v, want it to fall back to the after commit",
			gotIn["beforeCommitId"])
	}
}

// Permanence decides whether the scheduler keeps its dedup claim, so the split
// between "will never work" and "try later" has to be right.
func TestPostCodeCommitCommentPermanence(t *testing.T) {
	tests := []struct {
		faultType     string
		code          int
		wantPermanent bool
	}{
		{"PullRequestDoesNotExistException", http.StatusBadRequest, true},
		{"InvalidPullRequestIdException", http.StatusBadRequest, true},
		{"RepositoryDoesNotExistException", http.StatusBadRequest, true},
		{"CommitDoesNotExistException", http.StatusBadRequest, true},
		{"AccessDeniedException", http.StatusForbidden, true},
		{"ThrottlingException", http.StatusBadRequest, false},
		{"InternalServerException", http.StatusInternalServerError, false},
	}
	for _, tt := range tests {
		t.Run(tt.faultType, func(t *testing.T) {
			ccStub(t, func(string, map[string]any) (int, string) {
				return tt.code, `{"__type":"` + tt.faultType + `","message":"nope"}`
			})
			p := NewPoster()
			permanent, err := p.PostCodeCommitComment(context.Background(),
				CodeCommitRepo{Region: "ap-south-1", Name: "r"}, "", "1", "b", "a", "body")
			if err == nil {
				t.Fatal("expected an error")
			}
			if permanent != tt.wantPermanent {
				t.Errorf("permanent = %v, want %v (err %v)", permanent, tt.wantPermanent, err)
			}
			if !strings.Contains(err.Error(), tt.faultType[:10]) {
				t.Errorf("err = %v, want it to name the AWS error code", err)
			}
		})
	}
}

// A failure must not echo the response body, which can carry request material.
func TestPostCodeCommitCommentErrorLeaksNoBody(t *testing.T) {
	const secretish = "arn:aws:iam::123456789012:role/internal-only"
	ccStub(t, func(string, map[string]any) (int, string) {
		body, _ := json.Marshal(map[string]string{
			"__type":  "AccessDeniedException",
			"message": "User " + secretish + " is not authorized",
		})
		return http.StatusForbidden, string(body)
	})
	p := NewPoster()
	_, err := p.PostCodeCommitComment(context.Background(),
		CodeCommitRepo{Region: "ap-south-1", Name: "r"}, "", "1", "b", "a", "body")
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), secretish) {
		t.Errorf("error text leaked the response body: %v", err)
	}
}

// Regression: CodeCommit must not have disturbed the GitHub and Bitbucket
// status-request construction.
func TestPosterBuildUnchangedForHTTPSProviders(t *testing.T) {
	p := NewPoster()

	ghURL, ghBody, err := p.build(PostRequest{
		Provider: "github", Repo: "acme/app", SHA: "deadbeef",
		Status: "success", TargetURL: "https://forge.example.com/pipelines/1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if ghURL != "https://api.github.com/repos/acme/app/statuses/deadbeef" {
		t.Errorf("github URL = %q", ghURL)
	}
	if !strings.Contains(string(ghBody), `"state":"success"`) {
		t.Errorf("github body = %s", ghBody)
	}

	bbURL, bbBody, err := p.build(PostRequest{
		Provider: "bitbucket", Repo: "team/repo", SHA: "cafe",
		Status: "failed", TargetURL: "https://forge.example.com/pipelines/2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if bbURL != "https://api.bitbucket.org/2.0/repositories/team/repo/commit/cafe/statuses/build" {
		t.Errorf("bitbucket URL = %q", bbURL)
	}
	if !strings.Contains(string(bbBody), `"state":"FAILED"`) {
		t.Errorf("bitbucket body = %s", bbBody)
	}

	// CodeCommit never reaches build(): it is routed to the comment path first.
	if _, _, err := p.build(PostRequest{
		Provider: "codecommit", Repo: "r", SHA: "abc", Status: "success",
	}); err == nil {
		t.Error("build() should reject codecommit — it has no commit-status API")
	}
}
