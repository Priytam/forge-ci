package review

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
	"github.com/priytamjeepandey/forge-ci/internal/store"
	"github.com/priytamjeepandey/forge-ci/internal/vcs"
	"github.com/priytamjeepandey/forge-ci/internal/vcs/vcstest"
)

// TestOtherHostsEndToEnd is phase E's acceptance test: on GitLab, Gitea,
// Bitbucket and CodeCommit, the service holds a merge on the host until
// Forge's rules pass, a Forge comment reaches the host once and comes back
// as the same comment, and a merge from Forge merges the approved head.
func TestOtherHostsEndToEnd(t *testing.T) {
	hosts := []struct {
		provider string
		new      func(*testing.T) vcstest.Host
		reg      proto.RepoRegistration
	}{
		{"gitlab", func(t *testing.T) vcstest.Host { return vcstest.NewFakeGitLab(t) }, proto.RepoRegistration{}},
		{"gitea", func(t *testing.T) vcstest.Host { return vcstest.NewFakeGitea(t) }, proto.RepoRegistration{}},
		{"bitbucket", func(t *testing.T) vcstest.Host { return vcstest.NewFakeBitbucket(t) }, proto.RepoRegistration{}},
		{"codecommit", func(t *testing.T) vcstest.Host { return vcstest.NewFakeCodeCommit(t) }, proto.RepoRegistration{AWSRegion: "us-east-1"}},
	}
	for _, hc := range hosts {
		t.Run(hc.provider, func(t *testing.T) {
			h := newHarnessOn(t, hc.new(t), hc.reg)
			repo := h.host.Repo().Repo

			// The gate holds the host's own Merge button until it passes.
			id, changeID := h.gated("Seat lock")
			h.settle()
			head := h.host.Head(t, id)
			if got := h.host.Check(t, head, vcs.GateContext); got != "pending" {
				t.Fatalf("published gate = %q, want pending", got)
			}
			if err := h.host.HostMerge(t, id); err == nil {
				t.Fatal("the host merged while Forge's gate was pending")
			}
			h.approve(changeID, "ashwin@acme.io")
			h.settle()
			if got := h.host.Check(t, head, vcs.GateContext); got != "success" {
				t.Fatalf("gate after approval = %q, want success", got)
			}

			// A Forge line comment reaches the host once and its echo is
			// matched, not stored again.
			tid, _ := h.lineComment(changeID, "ashwin@acme.io", "Why not a map here?")
			h.settle()
			var posted int
			for _, c := range h.host.CommentsOn(t, id) {
				if strings.Contains(c, "Why not a map here?") {
					posted++
				}
			}
			if posted != 1 {
				t.Fatalf("on the host = %q (want the comment exactly once)", h.host.CommentsOn(t, id))
			}
			h.echo(id)
			ts := h.threads(changeID)
			if len(ts) != 1 || countComments(ts) != 1 || ts[0].ID != tid || ts[0].ExternalID == "" {
				t.Fatalf("after the echo = %+v", ts)
			}

			// Merge from Forge.
			payload, _ := json.Marshal(MergePayload{Method: "merge", ExpectedHead: head})
			cid := changeID
			if _, err := h.st.EnqueueOutbox(h.ctx, store.OutboxItem{IdemKey: "merge:" + hc.provider, Kind: KindMerge, Repo: repo,
				ChangeID: &cid, Class: vcs.ClassUser, Payload: payload, RequestedBy: "ashwin@acme.io"}, ""); err != nil {
				t.Fatal(err)
			}
			h.settle()
			acts, _ := h.st.ChangeActions(h.ctx, changeID, 10)
			if len(acts) == 0 || acts[0].Kind != KindMerge || acts[0].State != "done" {
				t.Fatalf("merge action = %+v", acts)
			}
			h.svc.RefreshDue(h.ctx)
			if d, _ := h.st.GetChangeDetail(h.ctx, changeID); d.State != "merged" {
				t.Errorf("Forge's copy = %s, want merged", d.State)
			}
		})
	}
}
