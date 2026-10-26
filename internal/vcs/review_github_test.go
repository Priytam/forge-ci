package vcs

import (
	"net/http"
	"os"
	"reflect"
	"testing"
	"time"
)

// TestGitHubNormalizeRecorded runs the GitHub change source over recorded
// webhook payloads (trimmed real deliveries), not just the conformance fake.
func TestGitHubNormalizeRecorded(t *testing.T) {
	src := ChangeSourceFor("github")
	load := func(name, event string) (http.Header, []byte) {
		body, err := os.ReadFile("testdata/github/" + name)
		if err != nil {
			t.Fatal(err)
		}
		hdr := http.Header{}
		hdr.Set("X-GitHub-Event", event)
		hdr.Set("X-GitHub-Delivery", "rec-"+name)
		return hdr, body
	}

	hdr, body := load("pull_request.closed_merged.json", "pull_request")
	evs, err := src.NormalizeEvent(hdr, body)
	if err != nil || len(evs) != 1 {
		t.Fatalf("merged: %+v %v", evs, err)
	}
	got := evs[0].Snapshot
	want := &Change{
		ID: "42", Title: "Seat-level lock for group checkout", Body: "Locks seats per traveller.\n\nCloses TRV-5014",
		Author: "harish-travafa", SourceBranch: "feat/seat-lock", TargetBranch: "dev", State: "merged",
		HeadSHA: "1a2b3c4d5e6f708192a3b4c5d6e7f80910111213", BaseSHA: "aabbccddeeff00112233445566778899aabbccdd",
		Labels: []string{"checkout", "p1"}, URL: "https://github.com/Letztrip/cos/pull/42",
		CreatedAt: time.Date(2026, 10, 20, 9, 12, 44, 0, time.UTC), UpdatedAt: time.Date(2026, 10, 21, 15, 2, 10, 0, time.UTC),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("merged snapshot =\n%+v\nwant\n%+v", got, want)
	}
	if evs[0].Repo != "Letztrip/cos" || evs[0].Kind != "change" || evs[0].Action != "closed" ||
		evs[0].DeliveryID != "rec-pull_request.closed_merged.json" {
		t.Errorf("merged event = %+v", evs[0])
	}

	hdr, body = load("pull_request.opened_draft.json", "pull_request")
	evs, err = src.NormalizeEvent(hdr, body)
	if err != nil || len(evs) != 1 || !evs[0].Snapshot.Draft || evs[0].Snapshot.State != "open" || evs[0].Snapshot.Body != "" {
		t.Errorf("draft: %+v %v", evs, err)
	}

	hdr, body = load("pull_request_review.submitted.json", "pull_request_review")
	evs, err = src.NormalizeEvent(hdr, body)
	if err != nil || len(evs) != 1 || evs[0].Kind != "review" || evs[0].ChangeID != "42" || evs[0].Snapshot != nil {
		t.Errorf("review: %+v %v (a review payload must only trigger a refresh)", evs, err)
	}

	// Comments are ignored until threads ship; pushes never concern a change.
	hdr, body = load("issue_comment.created.json", "issue_comment")
	if evs, err := src.NormalizeEvent(hdr, body); err != nil || len(evs) != 0 {
		t.Errorf("issue_comment: %+v %v", evs, err)
	}
}

func TestRateLimitedRecognition(t *testing.T) {
	reset := time.Now().Add(10 * time.Minute)
	h := http.Header{}
	h.Set("X-RateLimit-Remaining", "0")
	h.Set("X-RateLimit-Limit", "5000")
	h.Set("X-RateLimit-Reset", "1")
	// Primary limit exhausted: 403 with remaining 0 and no Retry-After waits
	// until the reset header.
	ri := parseRateHeaders(h)
	ri.Reset = reset
	rl := rateLimited(&http.Response{StatusCode: http.StatusForbidden, Header: h}, ri)
	if rl == nil || rl.RetryAfter < 9*time.Minute {
		t.Errorf("exhausted 403 = %+v", rl)
	}
	// A plain 403 (bad token scope) is not a rate limit.
	h2 := http.Header{}
	h2.Set("X-RateLimit-Remaining", "4000")
	if rl := rateLimited(&http.Response{StatusCode: http.StatusForbidden, Header: h2}, parseRateHeaders(h2)); rl != nil {
		t.Errorf("scope 403 treated as rate limit: %+v", rl)
	}
	if parseRateHeaders(http.Header{}) != nil {
		t.Error("no headers must mean no rate info")
	}
}

func TestGitHubGraphQLEndpoint(t *testing.T) {
	for in, want := range map[string]string{
		"https://api.github.com":     "https://api.github.com/graphql",
		"https://ghe.acme.io/api/v3": "https://ghe.acme.io/api/graphql",
		"http://127.0.0.1:5555":      "http://127.0.0.1:5555/graphql",
	} {
		if got := githubGraphQL(in); got != want {
			t.Errorf("githubGraphQL(%s) = %s, want %s", in, got, want)
		}
	}
}
