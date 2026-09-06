package api

import "testing"

// The commit author and the person who started a run are different facts, and
// the difference is only visible when a provider event is parsed correctly. A
// merge, a bot push or a re-run all have an actor who is not the author.

func TestRawAuthorName(t *testing.T) {
	tests := []struct{ in, want string }{
		{"Ada Lovelace <ada@example.com>", "Ada Lovelace"},
		{"  Ada Lovelace  <ada@example.com>  ", "Ada Lovelace"},
		// No angle-bracket form: keep whatever is there rather than guessing.
		{"ada", "ada"},
		{"", ""},
		// A header that is only an address has no name to extract.
		{"<ada@example.com>", "<ada@example.com>"},
	}
	for _, tt := range tests {
		if got := rawAuthorName(tt.in); got != tt.want {
			t.Errorf("rawAuthorName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// A GitHub push where the pusher merged someone else's work: the run is
// attributed to the pusher, the code to its author, and both must survive.
func TestGitHubPushCarriesCommitAuthorAndMessage(t *testing.T) {
	body := []byte(`{
		"ref": "refs/heads/main",
		"after": "9fceb02d",
		"repository": {"full_name": "acme/checkout"},
		"pusher": {"name": "alice"},
		"head_commit": {
			"message": "Fix booking race condition\n\nLonger body that the UI never shows.",
			"author": {"name": "Dana Kim", "username": "dana"}
		}
	}`)
	got, err := parseGitHubPush(body)
	if err != nil {
		t.Fatal(err)
	}
	if got.Actor != "alice" {
		t.Errorf("Actor = %q, want the pusher", got.Actor)
	}
	if got.CommitAuthor != "dana" {
		t.Errorf("CommitAuthor = %q, want the GitHub username", got.CommitAuthor)
	}
	if got.CommitMessage != "Fix booking race condition\n\nLonger body that the UI never shows." {
		t.Errorf("CommitMessage = %q, want the full message", got.CommitMessage)
	}
}

// A commit from an email GitHub cannot link to a user has no username; the git
// author name is the only thing available and must not be dropped.
func TestGitHubPushFallsBackToGitAuthorName(t *testing.T) {
	body := []byte(`{
		"ref": "refs/heads/main",
		"after": "abc",
		"repository": {"full_name": "acme/checkout"},
		"pusher": {"name": "alice"},
		"head_commit": {"message": "Bump deps", "author": {"name": "Dana Kim", "username": ""}}
	}`)
	got, err := parseGitHubPush(body)
	if err != nil {
		t.Fatal(err)
	}
	if got.CommitAuthor != "Dana Kim" {
		t.Errorf("CommitAuthor = %q, want the git author name", got.CommitAuthor)
	}
}

// A branch deletion carries no head_commit. Both fields stay empty rather than
// the parse failing or inventing an author.
func TestGitHubPushWithoutHeadCommit(t *testing.T) {
	body := []byte(`{
		"ref": "refs/heads/gone",
		"after": "0000000000000000000000000000000000000000",
		"repository": {"full_name": "acme/checkout"},
		"pusher": {"name": "alice"}
	}`)
	got, err := parseGitHubPush(body)
	if err != nil {
		t.Fatal(err)
	}
	if got.CommitAuthor != "" || got.CommitMessage != "" {
		t.Errorf("expected empty commit facts, got author=%q message=%q",
			got.CommitAuthor, got.CommitMessage)
	}
	if got.Actor != "alice" {
		t.Errorf("Actor = %q, want alice", got.Actor)
	}
}

// Bitbucket reports the author on the push target, preferring the linked
// account and falling back to the raw git header.
func TestBitbucketPushCommitAuthor(t *testing.T) {
	linked := []byte(`{
		"repository": {"full_name": "team/repo"},
		"actor": {"nickname": "alice"},
		"push": {"changes": [{"new": {
			"name": "main",
			"target": {
				"hash": "cafebabe",
				"message": "Tidy the migration",
				"author": {"raw": "Dana Kim <dana@example.com>", "user": {"nickname": "dana"}}
			}
		}}]}
	}`)
	got, ok, err := parseBitbucketPush(linked)
	if err != nil || !ok {
		t.Fatalf("parseBitbucketPush: ok=%v err=%v", ok, err)
	}
	if got.CommitAuthor != "dana" {
		t.Errorf("CommitAuthor = %q, want the linked nickname", got.CommitAuthor)
	}
	if got.CommitMessage != "Tidy the migration" {
		t.Errorf("CommitMessage = %q", got.CommitMessage)
	}
	if got.Actor != "alice" {
		t.Errorf("Actor = %q, want the pushing actor", got.Actor)
	}

	unlinked := []byte(`{
		"repository": {"full_name": "team/repo"},
		"actor": {"nickname": "alice"},
		"push": {"changes": [{"new": {
			"name": "main",
			"target": {
				"hash": "cafebabe",
				"message": "Tidy the migration",
				"author": {"raw": "Dana Kim <dana@example.com>", "user": {}}
			}
		}}]}
	}`)
	got2, ok2, err := parseBitbucketPush(unlinked)
	if err != nil || !ok2 {
		t.Fatalf("parseBitbucketPush: ok=%v err=%v", ok2, err)
	}
	if got2.CommitAuthor != "Dana Kim" {
		t.Errorf("CommitAuthor = %q, want the raw header name", got2.CommitAuthor)
	}
}

// A push with no changes is nothing to build, and must be distinguishable from
// a malformed payload.
func TestBitbucketPushNoChanges(t *testing.T) {
	_, ok, err := parseBitbucketPush([]byte(`{"repository": {"full_name": "team/repo"}, "push": {"changes": []}}`))
	if err != nil {
		t.Fatalf("an empty change list is not a parse error: %v", err)
	}
	if ok {
		t.Error("ok = true, want false for a push with no changes")
	}
	if _, _, err := parseBitbucketPush([]byte(`{not json`)); err == nil {
		t.Error("expected an error for malformed JSON")
	}
}
