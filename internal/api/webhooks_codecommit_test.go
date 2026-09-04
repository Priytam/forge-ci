package api

import "testing"

// A real "CodeCommit Repository State Change" EventBridge delivery.
const ccPushEvent = `{
	"version": "0",
	"id": "01234567-0123-0123-0123-012345678901",
	"detail-type": "CodeCommit Repository State Change",
	"source": "aws.codecommit",
	"account": "123456789012",
	"time": "2026-09-04T10:00:00Z",
	"region": "ap-south-1",
	"resources": ["arn:aws:codecommit:ap-south-1:123456789012:tablespace-api"],
	"detail": {
		"event": "referenceUpdated",
		"repositoryName": "tablespace-api",
		"repositoryId": "8a5f2b1c-0000-0000-0000-000000000000",
		"referenceType": "branch",
		"referenceName": "refs/heads/main",
		"commitId": "9fceb02d0ae598e95dc970b74767f19372d61af8",
		"oldCommitId": "0000000000000000000000000000000000000001",
		"callerUserArn": "arn:aws:iam::123456789012:user/alice"
	}
}`

// A real "CodeCommit Pull Request State Change" EventBridge delivery.
const ccPREvent = `{
	"version": "0",
	"id": "abcdefab-1111-2222-3333-444444444444",
	"detail-type": "CodeCommit Pull Request State Change",
	"source": "aws.codecommit",
	"region": "ap-south-1",
	"detail": {
		"event": "pullRequestCreated",
		"repositoryNames": ["tablespace-api"],
		"pullRequestId": "42",
		"title": "Add booking endpoint",
		"sourceReference": "refs/heads/feature/booking",
		"destinationReference": "refs/heads/main",
		"sourceCommit": "1111111111111111111111111111111111111111",
		"destinationCommit": "2222222222222222222222222222222222222222",
		"author": "arn:aws:iam::123456789012:user/bob",
		"pullRequestStatus": "Open",
		"isMerged": "False"
	}
}`

func TestCodeCommitEventKind(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"push on update", ccPushEvent, ccKindPush},
		{"pull request created", ccPREvent, ccKindPullRequest},
		{
			"push on branch create",
			`{"detail-type":"CodeCommit Repository State Change","detail":{"event":"referenceCreated"}}`,
			ccKindPush,
		},
		{
			"branch deletion ignored",
			`{"detail-type":"CodeCommit Repository State Change","detail":{"event":"referenceDeleted"}}`,
			ccKindIgnore,
		},
		{
			"PR source branch updated (AWS name)",
			`{"detail":{"event":"pullRequestSourceBranchUpdated"}}`,
			ccKindPullRequest,
		},
		{
			"PR source updated (abbreviated name)",
			`{"detail":{"event":"SourceUpdated"}}`,
			ccKindPullRequest,
		},
		{
			"PR merged/closed ignored",
			`{"detail":{"event":"pullRequestStatusChanged"}}`,
			ccKindIgnore,
		},
		{
			"unknown event falls back to detail-type",
			`{"detail-type":"CodeCommit Pull Request State Change","detail":{"event":"pullRequestSomethingNew"}}`,
			ccKindPullRequest,
		},
		{
			"spec-doc detail-type spellings",
			`{"detail-type":"Reference Changes","detail":{}}`,
			ccKindPush,
		},
		{"unrelated event ignored", `{"detail-type":"EC2 Instance State-change"}`, ccKindIgnore},
		{"malformed JSON ignored", `{not json`, ccKindIgnore},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := codeCommitEventKind([]byte(tt.body)); got != tt.want {
				t.Errorf("codeCommitEventKind = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseCodeCommitPush(t *testing.T) {
	push, err := parseCodeCommitPush([]byte(ccPushEvent))
	if err != nil {
		t.Fatal(err)
	}
	want := ccPush{
		Repo:   "tablespace-api",
		Ref:    "main",
		SHA:    "9fceb02d0ae598e95dc970b74767f19372d61af8",
		Author: "alice",
	}
	if push != want {
		t.Errorf("parseCodeCommitPush = %+v, want %+v", push, want)
	}
}

func TestParseCodeCommitPushTagRef(t *testing.T) {
	body := `{"detail":{"event":"referenceCreated","repositoryName":"r",
		"referenceType":"tag","referenceName":"refs/tags/v1.2.3","commitId":"deadbeef"}}`
	push, err := parseCodeCommitPush([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if push.Ref != "v1.2.3" {
		t.Errorf("Ref = %q, want %q", push.Ref, "v1.2.3")
	}
	if push.Author != "" {
		t.Errorf("Author = %q, want empty (event carries no caller)", push.Author)
	}
}

// A deletion carries the all-zero object id; there is nothing to build, and the
// handler must not treat the zero sha as a real commit.
func TestParseCodeCommitPushZeroSHA(t *testing.T) {
	body := `{"detail":{"event":"referenceDeleted","repositoryName":"r",
		"referenceName":"refs/heads/gone",
		"commitId":"0000000000000000000000000000000000000000"}}`
	push, err := parseCodeCommitPush([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if push.SHA != "" {
		t.Errorf("SHA = %q, want empty for the all-zero object id", push.SHA)
	}
}

func TestParseCodeCommitPR(t *testing.T) {
	pr, err := parseCodeCommitPR([]byte(ccPREvent))
	if err != nil {
		t.Fatal(err)
	}
	want := prTrigger{
		Repo:         "tablespace-api",
		IID:          42,
		Title:        "Add booking endpoint",
		SourceBranch: "feature/booking",
		TargetBranch: "main",
		HeadSHA:      "1111111111111111111111111111111111111111",
		Author:       "bob",
	}
	if pr != want {
		t.Errorf("parseCodeCommitPR = %+v, want %+v", pr, want)
	}
}

// Some CodeCommit PR events name the repo with the singular key rather than the
// repositoryNames array; both must resolve.
func TestParseCodeCommitPRSingularRepoKey(t *testing.T) {
	body := `{"detail":{"event":"pullRequestCreated","repositoryName":"solo-repo",
		"pullRequestId":"7","sourceReference":"refs/heads/x","destinationReference":"refs/heads/main",
		"sourceCommit":"aaa"}}`
	pr, err := parseCodeCommitPR([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if pr.Repo != "solo-repo" {
		t.Errorf("Repo = %q, want %q", pr.Repo, "solo-repo")
	}
	if pr.IID != 7 {
		t.Errorf("IID = %d, want 7", pr.IID)
	}
}

func TestParseCodeCommitPRMalformed(t *testing.T) {
	if _, err := parseCodeCommitPR([]byte(`{not json`)); err == nil {
		t.Error("expected an error for malformed JSON")
	}
	if _, err := parseCodeCommitPush([]byte(`{not json`)); err == nil {
		t.Error("expected an error for malformed JSON")
	}
}

func TestCodeCommitEventID(t *testing.T) {
	if got := codeCommitEventID([]byte(ccPushEvent)); got != "01234567-0123-0123-0123-012345678901" {
		t.Errorf("codeCommitEventID = %q", got)
	}
	// No envelope id -> "" so the caller falls back to a body hash.
	if got := codeCommitEventID([]byte(`{"detail":{}}`)); got != "" {
		t.Errorf("codeCommitEventID = %q, want empty", got)
	}
	if got := codeCommitEventID([]byte(`{not json`)); got != "" {
		t.Errorf("codeCommitEventID = %q, want empty", got)
	}
}

func TestARNPrincipal(t *testing.T) {
	tests := []struct{ in, want string }{
		{"arn:aws:iam::123456789012:user/alice", "alice"},
		{"arn:aws:sts::123456789012:assumed-role/DevRole/session-name", "session-name"},
		{"arn:aws:iam::123456789012:root", "root"},
		{"plain-username", "plain-username"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := arnPrincipal(tt.in); got != tt.want {
			t.Errorf("arnPrincipal(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestShortRefName(t *testing.T) {
	tests := []struct{ in, want string }{
		{"refs/heads/main", "main"},
		{"refs/heads/feature/nested/branch", "feature/nested/branch"},
		{"refs/tags/v1.0.0", "v1.0.0"},
		{"main", "main"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := shortRefName(tt.in); got != tt.want {
			t.Errorf("shortRefName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestIsZeroSHA(t *testing.T) {
	if !isZeroSHA("0000000000000000000000000000000000000000") {
		t.Error("all-zero sha should be reported as zero")
	}
	if isZeroSHA("") {
		t.Error("empty sha is not the zero object id")
	}
	if isZeroSHA("9fceb02d") {
		t.Error("real sha should not be reported as zero")
	}
}
