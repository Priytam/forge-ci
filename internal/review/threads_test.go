package review

import (
	"strings"
	"testing"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/blob"
	"github.com/priytamjeepandey/forge-ci/internal/proto"
	"github.com/priytamjeepandey/forge-ci/internal/store"
	"github.com/priytamjeepandey/forge-ci/internal/vcs"
	"github.com/priytamjeepandey/forge-ci/internal/vcs/vcstest"
)

// synced opens a pull request on the fake GitHub and syncs it into Forge.
func (h *harness) synced(title string) (hostID string, changeID int64) {
	h.t.Helper()
	id, _ := h.gh.Open(h.t, title)
	h.ingest(id, "change")
	h.svc.RefreshDue(h.ctx)
	for _, c := range h.open() {
		if c.Number == id {
			return id, c.ID
		}
	}
	h.t.Fatalf("change %s not synced", id)
	return "", 0
}

// echo delivers a comment webhook and runs the refresh it triggers.
func (h *harness) echo(id string) {
	h.t.Helper()
	h.ingest(id, "comment")
	h.svc.RefreshDue(h.ctx)
}

func (h *harness) threads(changeID int64) []store.ChangeThread {
	h.t.Helper()
	ts, err := h.st.ChangeThreads(h.ctx, changeID)
	if err != nil {
		h.t.Fatal(err)
	}
	return ts
}

func countComments(ts []store.ChangeThread) (n int) {
	for _, t := range ts {
		n += len(t.Comments)
	}
	return n
}

// lineComment saves a Forge line comment and queues it, as the API does.
func (h *harness) lineComment(changeID int64, who, body string) (threadID, commentID int64) {
	h.t.Helper()
	d, err := h.st.GetChangeDetail(h.ctx, changeID)
	if err != nil {
		h.t.Fatal(err)
	}
	tid, cid, _, err := h.st.CreateThread(h.ctx, changeID, store.NewThreadAnchor{Path: "app.go", Side: "new", Line: 3,
		CommitSHA: d.HeadSHA, AnchorHead: d.HeadSHA}, store.NewComment{Author: who, AuthorEmail: who, Body: body, Sync: true})
	if err != nil {
		h.t.Fatal(err)
	}
	if err := QueueComment(h.ctx, h.st, changeID, vcstestRepo, cid, who); err != nil {
		h.t.Fatal(err)
	}
	return tid, cid
}

// TestCommentRoundTripNoDuplicates is phase D's acceptance test: a comment
// written in Forge reaches GitHub once and comes back as the same comment —
// even when GitHub's answer is lost and the write is retried — and a comment
// written on GitHub arrives in Forge, both ways in one thread.
func TestCommentRoundTripNoDuplicates(t *testing.T) {
	h := newHarness(t)
	id, changeID := h.synced("Discuss")
	tid, _ := h.lineComment(changeID, "ashwin@acme.io", "Why not a map here?")
	if ts := h.threads(changeID); len(ts) != 1 || ts[0].Comments[0].State != "pending" {
		t.Fatalf("saved comment = %+v", ts)
	}

	// GitHub posts it, but the answer is lost: the write is retried.
	h.gh.LoseNextResponse()
	h.svc.SendOutbox(h.ctx)
	if _, err := h.db.Exec(h.ctx, `UPDATE provider_outbox SET next_attempt_at = now() WHERE state = 'pending'`); err != nil {
		t.Fatal(err)
	}
	h.svc.SendOutbox(h.ctx)
	on := h.gh.CommentsOn(t, id)
	if len(on) != 1 || !strings.HasPrefix(on[0], vcstest.BotLogin+": Why not a map here?\n\n— ashwin@acme.io via Forge\n\n<!-- forge:") {
		t.Fatalf("on GitHub = %q (want exactly one, by the bot, attributed, with the marker)", on)
	}
	ts := h.threads(changeID)
	if len(ts) != 1 || countComments(ts) != 1 || ts[0].Comments[0].State != "sent" {
		t.Fatalf("Forge after the retry = %+v", ts)
	}

	// The webhook echo is matched, not stored again.
	h.echo(id)
	ts = h.threads(changeID)
	if len(ts) != 1 || countComments(ts) != 1 || ts[0].ID != tid || ts[0].ExternalID == "" {
		t.Fatalf("after the echo = %+v", ts)
	}
	if body := ts[0].Comments[0].Body; body != "Why not a map here?" {
		t.Errorf("Forge shows %q (the marker and attribution must not leak back)", body)
	}

	// A reply on GitHub arrives in the same thread; a reply from Forge goes
	// to the same thread on GitHub.
	hostThread := ts[0].ExternalID
	h.ghReply(t, id, hostThread, "harish-gh", "A map would allocate.")
	h.echo(id)
	if ts := h.threads(changeID); countComments(ts) != 2 || ts[0].Comments[1].Author != "harish-gh" || ts[0].Comments[1].Source != "host" {
		t.Fatalf("host reply = %+v", ts)
	}
	cid, err := h.st.AddReply(h.ctx, changeID, tid, store.NewComment{Author: "ashwin@acme.io", AuthorEmail: "ashwin@acme.io", Body: "Fair.", Sync: true})
	if err != nil {
		t.Fatal(err)
	}
	_ = QueueComment(h.ctx, h.st, changeID, vcstestRepo, cid, "ashwin@acme.io")
	h.svc.SendOutbox(h.ctx)
	if _, n := h.gh.ThreadState(t, id, hostThread); n != 3 {
		t.Errorf("GitHub thread has %d comments, want 3", n)
	}
	h.echo(id)
	if ts := h.threads(changeID); len(ts) != 1 || countComments(ts) != 3 {
		t.Errorf("Forge thread = %+v", ts)
	}

	// Resolve in Forge reaches GitHub; reopening on GitHub reaches Forge.
	if err := h.st.SetThreadResolved(h.ctx, changeID, tid, true, "ashwin@acme.io", true); err != nil {
		t.Fatal(err)
	}
	_ = QueueResolve(h.ctx, h.st, changeID, vcstestRepo, tid, "ashwin@acme.io")
	h.echo(id) // a refresh before the outbox runs must not undo Forge's resolve
	if ts := h.threads(changeID); !ts[0].Resolved {
		t.Fatal("a refresh undid a resolve the host has not heard about yet")
	}
	h.svc.SendOutbox(h.ctx)
	if resolved, _ := h.gh.ThreadState(t, id, hostThread); !resolved {
		t.Fatal("resolve did not reach GitHub")
	}
	h.gh.HostResolve(t, id, hostThread, false)
	h.echo(id)
	if ts := h.threads(changeID); ts[0].Resolved {
		t.Error("reopening on GitHub did not reach Forge")
	}
}

// ghReply is someone replying in a thread on GitHub.
func (h *harness) ghReply(t *testing.T, id, threadID, login, body string) {
	t.Helper()
	h.gh.HostReply(t, id, threadID, login, body)
}

// TestLinkedUserWritesAsThemselves: with a linked account, comments and
// reviews are made with the user's own token (no "via Forge"), and the
// review counts on GitHub as theirs.
func TestLinkedUserWritesAsThemselves(t *testing.T) {
	h := newHarness(t)
	id, changeID := h.synced("Mine")
	h.gh.AddUser("gho_ashwin", "ashwin-gh")
	if err := h.st.LinkHostAccount(h.ctx, "ashwin@acme.io", "github", "ashwin-gh", "gho_ashwin", "repo"); err != nil {
		t.Fatal(err)
	}
	h.lineComment(changeID, "ashwin@acme.io", "Ship it")
	h.svc.SendOutbox(h.ctx)
	on := h.gh.CommentsOn(t, id)
	if len(on) != 1 || !strings.HasPrefix(on[0], "ashwin-gh: Ship it\n\n<!-- forge:") {
		t.Errorf("on GitHub = %q (want ashwin's own comment, no attribution line)", on)
	}
	head := h.gh.Head(t, id)
	if err := QueueReview(h.ctx, h.st, changeID, vcstestRepo, "ashwin@acme.io", "approve", "LGTM", head); err != nil {
		t.Fatal(err)
	}
	h.svc.SendOutbox(h.ctx)
	if got := h.gh.ReviewsBy(t, id, "ashwin-gh"); len(got) != 1 || got[0] != "APPROVED" {
		t.Errorf("reviews by ashwin = %v", got)
	}
	// Someone without a link: Forge does not approve on GitHub for them.
	_ = QueueReview(h.ctx, h.st, changeID, vcstestRepo, "dev@acme.io", "approve", "", head)
	h.svc.SendOutbox(h.ctx)
	if got := h.gh.ReviewsBy(t, id, vcstestBot); len(got) != 0 {
		t.Errorf("the bot approved on someone's behalf: %v", got)
	}
	links, _ := h.st.HostLinks(h.ctx, "ashwin@acme.io")
	if len(links) != 1 || links[0].Login != "ashwin-gh" {
		t.Errorf("links = %+v", links)
	}
	if email, _ := h.st.HostIdentityEmail(h.ctx, "github", "Ashwin-GH"); email != "ashwin@acme.io" {
		t.Errorf("linking did not map the login: %q", email)
	}
}

const vcstestBot = vcstest.BotLogin

// TestHostThreadsMirrored: threads written on GitHub appear, edits follow,
// deleted ones go, and a Forge thread whose first comment has not reached
// the host yet is left alone.
func TestHostThreadsMirrored(t *testing.T) {
	h := newHarness(t)
	id, changeID := h.synced("Mirror")
	h.gh.HostThread(t, id, "harish-gh", "Typo")
	h.lineComment(changeID, "ashwin@acme.io", "Not sent yet")
	h.echo(id)
	ts := h.threads(changeID)
	if len(ts) != 2 {
		t.Fatalf("threads = %+v", ts)
	}
	var host store.ChangeThread
	for _, th := range ts {
		if th.Source == "host" {
			host = th
		}
	}
	if host.Path != "app.go" || host.Line != 1 || host.Comments[0].Author != "harish-gh" {
		t.Errorf("host thread = %+v", host)
	}
	// A conversation comment written on GitHub lands in the conversation.
	h.gh.HostConversation(t, id, "harish-gh", "Ready to merge?")
	h.echo(id)
	conv := 0
	for _, th := range h.threads(changeID) {
		if th.Path == "" && len(th.Comments) == 1 && th.Comments[0].Body == "Ready to merge?" {
			conv++
		}
	}
	if conv != 1 {
		t.Errorf("conversation = %+v", h.threads(changeID))
	}
}

// TestReanchorOnPlainGit: on a host without native threads Forge keeps the
// thread itself and moves it to its line on the new head, from the diff
// between the two heads; a deleted line makes it outdated.
func TestReanchorOnPlainGit(t *testing.T) {
	h := newHarness(t)
	h.svc.blobs = blob.NewLocal(t.TempDir())
	h1, h2 := strings.Repeat("1", 40), strings.Repeat("2", 40)
	ch := &vcs.Change{ID: "1", Title: "native", Author: "dev", SourceBranch: "f", TargetBranch: "main", State: "open",
		HeadSHA: h1, BaseSHA: strings.Repeat("0", 40), UpdatedAt: time.Now()}
	changeID, _, err := h.st.ApplyChange(h.ctx, "acme/plain", "git", ch)
	if err != nil {
		t.Fatal(err)
	}
	kept, _, _, _ := h.st.CreateThread(h.ctx, changeID, store.NewThreadAnchor{Path: "app.go", Side: "new", Line: 3,
		CommitSHA: h1, AnchorHead: h1, Fingerprint: "three"}, store.NewComment{Author: "a", AuthorEmail: "a@x", Body: "keep", Sync: false})
	gone, _, _, _ := h.st.CreateThread(h.ctx, changeID, store.NewThreadAnchor{Path: "app.go", Side: "new", Line: 5,
		CommitSHA: h1, AnchorHead: h1, Fingerprint: "five"}, store.NewComment{Author: "a", AuthorEmail: "a@x", Body: "gone", Sync: false})
	ch.HeadSHA, ch.UpdatedAt = h2, time.Now().Add(time.Second)
	if _, _, err := h.st.ApplyChange(h.ctx, "acme/plain", "git", ch); err != nil {
		t.Fatal(err)
	}
	h.svc.ReanchorThreads(h.ctx) // asks for the h1..h2 diff
	k := store.DiffKey{Repo: "acme/plain", From: h1, To: h2, Mode: "range"}
	if d, err := h.st.GetDiff(h.ctx, k); err != nil || d.State != "pending" {
		t.Fatalf("range diff not requested: %+v %v", d, err)
	}
	patch := "diff --git a/app.go b/app.go\n--- a/app.go\n+++ b/app.go\n@@ -1,5 +1,6 @@\n one\n+zero\n two\n three\n four\n-five\n"
	if err := StoreDiff(h.ctx, h.st, h.svc.blobs, k, &proto.DiffResult{MergeBase: h1,
		Files: []proto.DiffFile{{Path: "app.go", Status: "modified", Additions: 1, Deletions: 1, Patch: patch}}}); err != nil {
		t.Fatal(err)
	}
	if n := h.svc.ReanchorThreads(h.ctx); n != 2 {
		t.Fatalf("re-anchored %d threads, want 2", n)
	}
	for _, th := range h.threads(changeID) {
		switch th.ID {
		case kept:
			if th.Line != 4 || th.Outdated || th.CommitSHA != h2 {
				t.Errorf("kept thread = %+v (want line 4 on the new head)", th)
			}
		case gone:
			if !th.Outdated || th.Line != 0 {
				t.Errorf("thread on a deleted line = %+v (want outdated)", th)
			}
		}
		if th.Comments[0].State != "local" {
			t.Errorf("a plain-git comment is %q, want local (never sent anywhere)", th.Comments[0].State)
		}
	}
	if n := h.svc.ReanchorThreads(h.ctx); n != 0 {
		t.Errorf("re-anchored again (%d)", n)
	}
}
