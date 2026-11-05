package vcs_test

import (
	"context"
	"testing"

	"github.com/priytamjeepandey/forge-ci/internal/vcs"
	"github.com/priytamjeepandey/forge-ci/internal/vcs/vcstest"
)

// Every code-review plugin runs the same conformance suite. A provider joins
// this list when it implements vcs.ChangeSource.
func TestChangeSourceConformance(t *testing.T) {
	harnesses := []vcstest.Harness{
		{Provider: "github", New: func(t *testing.T) vcstest.Host { return vcstest.NewFakeGitHub(t) }},
		{Provider: "git", New: func(t *testing.T) vcstest.Host { return vcstest.NewFakeGit(t) }},
		{Provider: "gitlab", New: func(t *testing.T) vcstest.Host { return vcstest.NewFakeGitLab(t) }},
		{Provider: "gitea", New: func(t *testing.T) vcstest.Host { return vcstest.NewFakeGitea(t) }},
		{Provider: "bitbucket", New: func(t *testing.T) vcstest.Host { return vcstest.NewFakeBitbucket(t) }},
		{Provider: "codecommit", New: func(t *testing.T) vcstest.Host { return vcstest.NewFakeCodeCommit(t) }},
	}
	covered := map[string]bool{}
	for _, h := range harnesses {
		covered[h.Provider] = true
		t.Run(h.Provider, func(t *testing.T) { vcstest.RunChangeSource(t, h) })
	}
	// A plugin that implements ChangeSource without a harness here would ship
	// untested: fail instead.
	for _, name := range vcs.Names() {
		if vcs.ChangeSourceFor(name) != nil && !covered[name] {
			t.Errorf("provider %q implements ChangeSource but has no conformance harness", name)
		}
	}
}

// TestCodeCommitSpecifics: a retried comment carries the same client request
// token and stays one comment; Forge's own approval (the gate) is not read
// back as somebody's review.
func TestCodeCommitSpecifics(t *testing.T) {
	host := vcstest.NewFakeCodeCommit(t)
	c, ctx := vcs.NewChangeClient(), context.Background()
	repo := host.Repo()
	id, _ := host.Open(t, "x")
	w := vcs.ChangeWriterFor("codecommit")
	tw := vcs.ThreadWriterFor("codecommit")
	body := vcs.Mark("once", "0011223344556677")
	a, err := tw.PostComment(ctx, c, repo, id, vcs.CommentRequest{Body: body})
	if err != nil {
		t.Fatal(err)
	}
	b, err := tw.PostComment(ctx, c, repo, id, vcs.CommentRequest{Body: body})
	if err != nil || a != b || len(host.CommentsOn(t, id)) != 1 {
		t.Errorf("retried post: %q vs %q (%v), comments %v", a, b, err, host.CommentsOn(t, id))
	}
	if err := w.PublishCheck(ctx, c, repo, host.Head(t, id), vcs.CheckResult{State: "success", ChangeID: id}); err != nil {
		t.Fatal(err)
	}
	host.Review(t, id, "ashwin", "approve")
	ch, err := vcs.ChangeSourceFor("codecommit").GetChange(ctx, c, repo, id, nil)
	if err != nil || len(ch.Reviews) != 1 || ch.Reviews[0].Reviewer != "ashwin" {
		t.Errorf("reviews = %+v %v (Forge's gate approval must not count as a review)", ch.Reviews, err)
	}
}
