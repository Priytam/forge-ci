package review

import (
	"strings"
	"testing"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/store"
)

func gateIn(reviews ...store.ChangeReview) *store.GateInputs {
	return &store.GateInputs{
		Change: store.ReviewTargetChange{ID: 1, Repo: "acme/shop", Provider: "github", Number: "7",
			Author: "harish-gh", TargetBranch: "main", State: "open", HeadSHA: "head2"},
		Title: "TRV-5014 seat lock", Source: "feat/seat-lock", Labels: []string{"checkout"},
		Reviews:    reviews,
		Pipeline:   &store.HeadPipeline{ID: 9, Status: "success", Jobs: map[string]string{"test": "success"}},
		Identities: map[string]string{"harish-gh": "harish@acme.io", "ashwin-gh": "ashwin@acme.io", "dev-gh": "dev@acme.io"},
	}
}

var t0 = time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC) // a Wednesday

func rv(source, who, verdict, sha string, at int) store.ChangeReview {
	return store.ChangeReview{Source: source, Reviewer: who, Verdict: verdict, CommitSHA: sha,
		SubmittedAt: t0.Add(time.Duration(at) * time.Minute)}
}

func rule(v Verdict, id string) store.GateRule {
	for _, r := range v.Rules {
		if r.ID == id {
			return r
		}
	}
	return store.GateRule{}
}

func TestGateApprovalsCountOncePerPersonAndRespectRules(t *testing.T) {
	p := &store.MergePolicy{RequiredApprovals: 2, DismissOnPush: true, BlockOnChangesRequested: true}
	el := Eligibility{Eligible: map[string]bool{"ashwin@acme.io": true, "dev@acme.io": true, "harish@acme.io": true}}
	in := gateIn(
		rv("host", "ashwin-gh", "approve", "head2", 1),
		rv("forge", "ashwin@acme.io", "approve", "head2", 2), // same person in Forge: counts once
		rv("host", "harish-gh", "approve", "head2", 3),       // the author
		rv("host", "dev-gh", "approve", "head1", 4),          // before the last push
		rv("host", "stranger", "approve", "head2", 5),        // not linked
	)
	v := Evaluate(in, p, el, t0)
	r := rule(v, "approvals")
	if v.State != "pending" || r.State != "pending" || !strings.HasPrefix(r.Detail, "1 of 2 approvals") {
		t.Fatalf("verdict = %+v", v)
	}
	for _, why := range []string{"own change", "earlier revision", "not linked"} {
		if !strings.Contains(r.Detail, why) {
			t.Errorf("detail %q does not explain %q", r.Detail, why)
		}
	}
	// dev re-approves the new head; the gate passes.
	in.Reviews = append(in.Reviews, rv("forge", "dev@acme.io", "approve", "head2", 6))
	if v := Evaluate(in, p, el, t0); v.State != "pass" || v.Summary != "All 2 merge rules pass" {
		t.Errorf("after re-approval = %+v", v)
	}
	// Self-approval allowed by the rule; bootstrap repos count unlinked logins.
	p2 := *p
	p2.AllowSelfApproval, p2.DismissOnPush = true, false
	if r := rule(Evaluate(gateIn(rv("host", "harish-gh", "approve", "head1", 1), rv("host", "x", "approve", "", 2)), &p2,
		Eligibility{Bootstrap: true}, t0), "approvals"); r.State != "pass" {
		t.Errorf("self + bootstrap = %+v", r)
	}
	// No approver role.
	if r := rule(Evaluate(gateIn(rv("forge", "dev@acme.io", "approve", "head2", 1)), &store.MergePolicy{RequiredApprovals: 1},
		Eligibility{Eligible: map[string]bool{}}, t0), "approvals"); r.State != "pending" || !strings.Contains(r.Detail, "no approver role") {
		t.Errorf("roleless approval = %+v", r)
	}
}

func TestGateChangesRequestedGroupAndWithdrawal(t *testing.T) {
	p := &store.MergePolicy{RequiredApprovals: 1, RequiredGroup: "release", RequiredGroupApprovals: 1, BlockOnChangesRequested: true}
	el := Eligibility{Eligible: map[string]bool{"ashwin@acme.io": true, "dev@acme.io": true}, Group: map[string]bool{"ashwin@acme.io": true}}
	in := gateIn(rv("host", "dev-gh", "approve", "head2", 1), rv("host", "ashwin-gh", "request_changes", "head2", 2))
	v := Evaluate(in, p, el, t0)
	if v.State != "fail" || rule(v, "changes_requested").State != "fail" || rule(v, "group").Detail != "0 of 1 from release" {
		t.Fatalf("verdict = %+v", v)
	}
	// Ashwin approves after requesting changes: the latest verdict wins.
	in.Reviews = append(in.Reviews, rv("forge", "ashwin@acme.io", "approve", "head2", 3))
	if v := Evaluate(in, p, el, t0); v.State != "pass" {
		t.Errorf("after approval = %+v", v)
	}
	// Withdrawing (dismissed) removes it again.
	in.Reviews = append(in.Reviews, rv("forge", "ashwin@acme.io", "dismissed", "head2", 4))
	if v := Evaluate(in, p, el, t0); v.State != "pending" || rule(v, "group").State != "pending" {
		t.Errorf("after withdrawal = %+v", v)
	}
}

func TestGateCIJobsLabelsTicketDraft(t *testing.T) {
	p := &store.MergePolicy{RequireCI: true, RequiredJobs: []string{"test"}, RequiredLabels: []string{"qa-ok"},
		BlockedLabels: []string{"do-not-merge"}, TicketPattern: `TRV-[0-9]+`}
	in := gateIn()
	v := Evaluate(in, p, Eligibility{}, t0)
	if rule(v, "ci").State != "pass" || rule(v, "ticket").State != "pass" || rule(v, "labels").State != "pending" {
		t.Fatalf("verdict = %+v", v)
	}
	in.Labels = []string{"QA-OK", "do-not-merge"}
	if r := rule(Evaluate(in, p, Eligibility{}, t0), "labels"); r.State != "fail" || r.Detail != "Labelled do-not-merge" {
		t.Errorf("blocked label = %+v", r)
	}
	in.Labels = []string{"qa-ok"}
	in.Title = "seat lock"
	if r := rule(Evaluate(in, p, Eligibility{}, t0), "ticket"); r.State != "fail" {
		t.Errorf("missing ticket = %+v", r)
	}
	in.Source = "TRV-12-fix"
	for _, c := range []struct {
		pl   *store.HeadPipeline
		want string
	}{
		{nil, "pending"},
		{&store.HeadPipeline{ID: 3, Status: "running", Jobs: map[string]string{"test": "running"}}, "pending"},
		{&store.HeadPipeline{ID: 3, Status: "failed", Jobs: map[string]string{"test": "success"}}, "fail"},
		{&store.HeadPipeline{ID: 3, Status: "success", Jobs: map[string]string{"lint": "success"}}, "fail"},
		{&store.HeadPipeline{ID: 3, Status: "blocked", Jobs: map[string]string{"test": "success"}}, "pending"},
	} {
		in.Pipeline = c.pl
		if r := rule(Evaluate(in, p, Eligibility{}, t0), "ci"); r.State != c.want {
			t.Errorf("pipeline %+v: ci = %+v, want %s", c.pl, r, c.want)
		}
	}
	in.Pipeline = &store.HeadPipeline{ID: 3, Status: "success", Jobs: map[string]string{"test": "success"}}
	in.Change.Draft = true
	if v := Evaluate(in, p, Eligibility{}, t0); v.State != "pending" || v.Summary != "The change is a draft" {
		t.Errorf("draft = %+v", v)
	}
}

func TestGateFreezeWindows(t *testing.T) {
	p := &store.MergePolicy{Timezone: "Asia/Kolkata",
		FreezeWindows: []store.FreezeWindow{{Start: "fri 18:00", End: "mon 09:00"}}}
	ist, _ := time.LoadLocation("Asia/Kolkata")
	sat := time.Date(2026, 10, 24, 12, 0, 0, 0, ist)
	v := Evaluate(gateIn(), p, Eligibility{}, sat)
	if v.State != "pending" || !strings.Contains(v.Summary, "Mon 09:00") {
		t.Fatalf("saturday = %+v", v)
	}
	if want := time.Date(2026, 10, 26, 9, 0, 0, 0, ist); v.RecheckAt == nil || !v.RecheckAt.Equal(want) {
		t.Errorf("recheck = %v, want %v", v.RecheckAt, want)
	}
	wed := time.Date(2026, 10, 21, 12, 0, 0, 0, ist)
	v = Evaluate(gateIn(), p, Eligibility{}, wed)
	if v.State != "pass" || !v.RecheckAt.Equal(time.Date(2026, 10, 23, 18, 0, 0, 0, ist)) {
		t.Errorf("wednesday = %+v (recheck %v)", v, v.RecheckAt)
	}
	// A window within a day.
	p.FreezeWindows = []store.FreezeWindow{{Start: "wed 11:00", End: "wed 13:00"}}
	if v := Evaluate(gateIn(), p, Eligibility{}, wed); v.State != "pending" {
		t.Errorf("same-day window = %+v", v)
	}
}

func TestPolicyValidateAndMatch(t *testing.T) {
	bad := []store.MergePolicy{
		{TicketPattern: "("},
		{Timezone: "Mars/Olympus"},
		{FreezeWindows: []store.FreezeWindow{{Start: "funday 10:00", End: "mon 10:00"}}},
		{RequiredGroupApprovals: 1},
		{ApproverRoles: []string{"guest"}},
	}
	for _, p := range bad {
		if err := p.Validate(); err == nil {
			t.Errorf("accepted %+v", p)
		}
	}
	ps := []store.MergePolicy{
		{ID: 1, Repo: "", BranchPattern: "*"},
		{ID: 2, Repo: "", BranchPattern: "main"},
		{ID: 3, Repo: "acme/shop", BranchPattern: "*"},
		{ID: 4, Repo: "acme/shop", BranchPattern: "release/*"},
	}
	for _, c := range []struct {
		repo, branch string
		want         int64
	}{
		{"acme/shop", "release/1.2", 4}, {"acme/shop", "main", 3}, {"acme/web", "main", 2}, {"acme/web", "dev", 1},
	} {
		if got := store.MatchMergePolicy(ps, c.repo, c.branch); got == nil || got.ID != c.want {
			t.Errorf("%s %s → %+v, want %d", c.repo, c.branch, got, c.want)
		}
	}
	if store.MatchMergePolicy(ps[3:], "acme/shop", "main") != nil {
		t.Error("a rule matched a branch outside its pattern")
	}
}
