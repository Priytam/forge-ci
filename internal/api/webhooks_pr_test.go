package api

import (
	"testing"

	"github.com/priytamjeepandey/forge-ci/internal/compiler"
)

func TestParseGitHubPR(t *testing.T) {
	body := []byte(`{
		"action": "opened",
		"number": 7,
		"pull_request": {
			"title": "Add feature",
			"head": {"sha": "headsha123", "ref": "feature/x"},
			"base": {"ref": "main"},
			"user": {"login": "octocat"}
		},
		"repository": {"full_name": "Priytam/statemachine"}
	}`)
	pr, err := parseGitHubPR(body)
	if err != nil {
		t.Fatal(err)
	}
	want := prTrigger{
		Action: "opened", Repo: "Priytam/statemachine", IID: 7, Title: "Add feature",
		SourceBranch: "feature/x", TargetBranch: "main", HeadSHA: "headsha123", Author: "octocat",
	}
	if pr != want {
		t.Errorf("parseGitHubPR = %+v, want %+v", pr, want)
	}
}

func TestParseBitbucketPR(t *testing.T) {
	body := []byte(`{
		"repository": {"full_name": "team/repo"},
		"actor": {"nickname": "bob", "display_name": "Bob B"},
		"pullrequest": {
			"id": 3,
			"title": "Fix bug",
			"source": {"branch": {"name": "bugfix"}, "commit": {"hash": "abcdef"}},
			"destination": {"branch": {"name": "develop"}}
		}
	}`)
	pr, err := parseBitbucketPR(body)
	if err != nil {
		t.Fatal(err)
	}
	want := prTrigger{
		Repo: "team/repo", IID: 3, Title: "Fix bug",
		SourceBranch: "bugfix", TargetBranch: "develop", HeadSHA: "abcdef", Author: "bob",
	}
	if pr != want {
		t.Errorf("parseBitbucketPR = %+v, want %+v", pr, want)
	}
}

func TestParseBitbucketPRAuthorFallback(t *testing.T) {
	body := []byte(`{
		"repository": {"full_name": "team/repo"},
		"actor": {"display_name": "Only Display"},
		"pullrequest": {"id": 1, "source": {"branch": {"name": "b"}, "commit": {"hash": "h"}}, "destination": {"branch": {"name": "main"}}}
	}`)
	pr, err := parseBitbucketPR(body)
	if err != nil {
		t.Fatal(err)
	}
	if pr.Author != "Only Display" {
		t.Errorf("author = %q, want display_name fallback", pr.Author)
	}
}

func TestPRActionTriggers(t *testing.T) {
	trigger := []string{"opened", "synchronize", "reopened"}
	ignore := []string{"closed", "merged", "edited", "labeled", "assigned", ""}
	for _, a := range trigger {
		if !prActionTriggers(a) {
			t.Errorf("action %q should trigger", a)
		}
	}
	for _, a := range ignore {
		if prActionTriggers(a) {
			t.Errorf("action %q should be ignored", a)
		}
	}
}

func TestMergeRequestContext(t *testing.T) {
	ctx := mergeRequestContext(42, "feature/x", "main", "My Title")
	want := map[string]string{
		"CI_PIPELINE_SOURCE":             compiler.SourceMergeRequest,
		"CI_MERGE_REQUEST_IID":           "42",
		"CI_MERGE_REQUEST_SOURCE_BRANCH": "feature/x",
		"CI_MERGE_REQUEST_TARGET_BRANCH": "main",
		"CI_MERGE_REQUEST_TITLE":         "My Title",
	}
	for k, v := range want {
		if ctx[k] != v {
			t.Errorf("ctx[%q] = %q, want %q", k, ctx[k], v)
		}
	}
}
