package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
	"github.com/priytamjeepandey/forge-ci/internal/vcs"
)

func reviewChange(id, head string, updated time.Time) *vcs.Change {
	return &vcs.Change{ID: id, Title: "Add seat lock", Author: "harish", SourceBranch: "feat/x",
		TargetBranch: "main", State: "open", HeadSHA: head, BaseSHA: "base1", UpdatedAt: updated}
}

// TestApplyChangeOrderingRevisionsReviews: an older webhook never overwrites a
// newer copy, every new head is a numbered revision, and a refresh's review
// list replaces the host's reviews (Forge's own are kept).
func TestApplyChangeOrderingRevisionsReviews(t *testing.T) {
	st := newArtifactsTestStore(t)
	ctx := context.Background()
	t0 := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)

	id, ok, err := st.ApplyChange(ctx, "acme/shop", "github", reviewChange("7", "h1", t0.Add(time.Minute)))
	if err != nil || !ok {
		t.Fatalf("apply: %v %v", ok, err)
	}
	if _, ok, err := st.ApplyChange(ctx, "acme/shop", "github", reviewChange("7", "h0", t0)); err != nil || ok {
		t.Fatalf("stale apply written: ok=%v err=%v", ok, err)
	}
	c2 := reviewChange("7", "h2", t0.Add(2*time.Minute))
	c2.Reviews = []vcs.Review{
		{ExternalID: "r1", Reviewer: "ashwin", Verdict: "approve", SubmittedAt: t0},
		{ExternalID: "r2", Reviewer: "bob", Verdict: "request_changes", SubmittedAt: t0},
	}
	if _, _, err := st.ApplyChange(ctx, "acme/shop", "github", c2); err != nil {
		t.Fatal(err)
	}
	if _, err := st.pool.Exec(ctx,
		`INSERT INTO change_reviews (change_id, source, external_id, reviewer, verdict) VALUES ($1,'forge','f1','carol','approve')`, id); err != nil {
		t.Fatal(err)
	}
	// Bob's review is dismissed on the host: the next refresh drops it.
	c3 := reviewChange("7", "h2", t0.Add(3*time.Minute))
	c3.Reviews = []vcs.Review{{ExternalID: "r1", Reviewer: "ashwin", Verdict: "approve", SubmittedAt: t0}}
	if _, _, err := st.ApplyChange(ctx, "acme/shop", "github", c3); err != nil {
		t.Fatal(err)
	}
	d, err := st.GetChangeDetail(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if d.HeadSHA != "h2" || d.Revisions != 2 || d.RevisionList[0].Number != 2 || d.RevisionList[1].HeadSHA != "h1" {
		t.Errorf("revisions = %+v (head %s)", d.RevisionList, d.HeadSHA)
	}
	if len(d.Reviews) != 2 || d.Approvals != 2 || d.ChangesWanted != 0 {
		t.Errorf("reviews = %+v approvals=%d wanted=%d", d.Reviews, d.Approvals, d.ChangesWanted)
	}
	list, total, err := st.ListChanges(ctx, ChangeQuery{})
	if err != nil || total != 1 || list[0].ID != id {
		t.Errorf("inbox = %+v %d %v", list, total, err)
	}
	if _, _, err := st.ListChanges(ctx, ChangeQuery{State: "bogus"}); err == nil {
		t.Error("bad state accepted")
	}
}

// TestRefreshCoalescing: a burst of requests is one refresh; a request while
// a refresh is in flight leaves exactly one more; placeholders stay off the
// inbox until their first sync.
func TestRefreshCoalescing(t *testing.T) {
	st := newArtifactsTestStore(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := st.RequestChangeRefresh(ctx, "acme/shop", "github", "9", PriorityReconcile, 0, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.RequestChangeRefresh(ctx, "acme/shop", "github", "9", PriorityWebhook, time.Hour, true); err != nil {
		t.Fatal(err)
	}
	if list, _, _ := st.ListChanges(ctx, ChangeQuery{}); len(list) != 0 {
		t.Errorf("placeholder listed: %+v", list)
	}
	tasks, err := st.ClaimChangeRefreshes(ctx, 10, time.Minute)
	if err != nil || len(tasks) != 1 || tasks[0].Priority != PriorityWebhook {
		t.Fatalf("claim = %+v %v (want one, raised to webhook priority, due time kept)", tasks, err)
	}
	if again, _ := st.ClaimChangeRefreshes(ctx, 10, time.Minute); len(again) != 0 {
		t.Fatalf("leased refresh claimed twice: %+v", again)
	}
	// An event lands mid-refresh: one more refresh stays queued.
	time.Sleep(5 * time.Millisecond)
	if err := st.RequestChangeRefresh(ctx, "acme/shop", "github", "9", PriorityWebhook, 0, true); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishChangeRefresh(ctx, tasks[0], nil, time.Time{}); err != nil {
		t.Fatal(err)
	}
	next, _ := st.ClaimChangeRefreshes(ctx, 10, time.Minute)
	if len(next) != 1 {
		t.Fatalf("follow-up refresh = %+v, want 1", next)
	}
	if err := st.FinishChangeRefresh(ctx, next[0], nil, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if n, _, _ := st.RefreshBacklog(ctx); n != 0 {
		t.Errorf("backlog = %d after the follow-up, want 0", n)
	}
	// Failure keeps the error and reschedules.
	_ = st.RequestChangeRefresh(ctx, "acme/shop", "github", "9", PriorityWebhook, 0, true)
	f, _ := st.ClaimChangeRefreshes(ctx, 10, time.Minute)
	if err := st.FinishChangeRefresh(ctx, f[0], errors.New("HTTP 502"), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var attempts int
	var msg string
	_ = st.pool.QueryRow(ctx, `SELECT refresh_attempts, refresh_error FROM changes WHERE number='9'`).Scan(&attempts, &msg)
	if attempts != 1 || msg != "HTTP 502" {
		t.Errorf("after failure attempts=%d err=%q", attempts, msg)
	}
	// A change the host never knew is dropped, placeholder and all.
	if err := st.DropChangeRefresh(ctx, f[0], "gone"); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = st.pool.QueryRow(ctx, `SELECT count(*) FROM changes`).Scan(&n)
	if n != 0 {
		t.Errorf("placeholder not deleted (%d rows)", n)
	}
}

// TestProviderBudgetPriorities: low classes stop first as the bucket drains;
// user actions only stop at zero; host headers overwrite the guess; a host
// window reset refills it.
func TestProviderBudgetPriorities(t *testing.T) {
	st := newArtifactsTestStore(t)
	ctx := context.Background()
	reset := time.Now().Add(20 * time.Minute)
	// 1,000 of 5,000 left: below the 20% reconcile reserve, above webhook's 5%.
	if err := st.ObserveProviderRate(ctx, "github-app:1", "github", vcs.RateInfo{Remaining: 1000, Limit: 5000, Reset: reset}); err != nil {
		t.Fatal(err)
	}
	for class, want := range map[string]bool{
		vcs.ClassBackfill: false, vcs.ClassReconcile: false, vcs.ClassWebhook: true, vcs.ClassUser: true,
	} {
		ok, wait, err := st.AcquireProviderBudget(ctx, "github-app:1", "github", class)
		if err != nil || ok != want {
			t.Errorf("%s: ok=%v err=%v, want %v", class, ok, err, want)
		}
		if !ok && (wait < 19*time.Minute || wait > 21*time.Minute) {
			t.Errorf("%s waits %s, want until the host's reset", class, wait)
		}
	}
	if err := st.ExhaustProviderBudget(ctx, "github-app:1", "github", time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if ok, _, _ := st.AcquireProviderBudget(ctx, "github-app:1", "github", vcs.ClassUser); ok {
		t.Error("user call allowed on an exhausted bucket")
	}
	// The host's window has reset: full again.
	if _, err := st.pool.Exec(ctx, `UPDATE provider_budgets SET reset_at = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if ok, _, _ := st.AcquireProviderBudget(ctx, "github-app:1", "github", vcs.ClassBackfill); !ok {
		t.Error("backfill refused after the window reset")
	}
	b, _ := st.ListProviderBudgets(ctx)
	if len(b) != 1 || b[0].Remaining != 4999 || b[0].Capacity != 5000 {
		t.Errorf("budgets = %+v", b)
	}
	// No headers yet: continuous refill from the provider default.
	if ok, _, err := st.AcquireProviderBudget(ctx, "git:anon:example", "git", vcs.ClassReconcile); err != nil || !ok {
		t.Errorf("fresh bucket refused: %v %v", ok, err)
	}
}

func TestProviderCallStats(t *testing.T) {
	st := newArtifactsTestStore(t)
	ctx := context.Background()
	for _, c := range []vcs.CallInfo{
		{Provider: "github", Class: "reconcile", Result: "not_modified"},
		{Provider: "github", Class: "reconcile", Result: "not_modified"},
		{Provider: "github", Class: "webhook", Result: "ok"},
	} {
		if err := st.RecordProviderCall(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.ProviderCallsSince(ctx, time.Now().Add(-time.Hour))
	if err != nil || len(got) != 2 || got[0].Calls != 2 || got[0].Result != "not_modified" {
		t.Errorf("stats = %+v %v", got, err)
	}
}

// TestReviewTargetAndReadPath: credentials map to budget keys (never the
// token itself), and resolving one on the read path is refused and counted.
func TestReviewTargetAndReadPath(t *testing.T) {
	t.Setenv("FORGE_SECRET_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	st := newArtifactsTestStore(t)
	ctx := context.Background()
	if err := st.RegisterRepo(ctx, proto.RepoRegistration{Repo: "acme/shop", Provider: "github",
		CloneURL: "https://github.com/acme/shop.git", Token: "ghp_secret", DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	if err := st.RegisterRepo(ctx, proto.RepoRegistration{Repo: "acme/plain", Provider: "git",
		CloneURL: "https://git.acme.io/plain.git", DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	ref, cred, found, err := st.ReviewTarget(ctx, "acme/shop")
	if err != nil || !found || ref.Token != "ghp_secret" || cred == "" || cred == "ghp_secret" ||
		len(cred) != len("github:token:")+12 {
		t.Errorf("github target = %+v %q %v %v", ref, cred, found, err)
	}
	if _, cred, _, _ := st.ReviewTarget(ctx, "acme/plain"); cred != "git:anon:git.acme.io" {
		t.Errorf("anon credential = %q", cred)
	}
	if _, _, _, err := st.ReviewTarget(vcs.ReadPath(ctx), "acme/shop"); !errors.Is(err, vcs.ErrReadPath) {
		t.Errorf("read-path credential err = %v, want ErrReadPath", err)
	}
	stats, _ := st.ProviderCallsSince(ctx, time.Now().Add(-time.Hour))
	if len(stats) != 1 || !stats[0].ReadPath || stats[0].Result != "refused" {
		t.Errorf("read-path refusal not counted: %+v", stats)
	}
}
