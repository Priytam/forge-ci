package review

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/priytamjeepandey/forge-ci/internal/proto"
	"github.com/priytamjeepandey/forge-ci/internal/store"
	"github.com/priytamjeepandey/forge-ci/internal/vcs"
	"github.com/priytamjeepandey/forge-ci/internal/vcs/vcstest"
)

func baseDSN() string {
	if v := os.Getenv("DATABASE_URL"); v != "" {
		return v
	}
	return "postgres://forge:forge@localhost:5433/forge?sslmode=disable"
}

// harness is a throwaway DB with acme/shop connected to a fake GitHub, and a
// service whose debounce is zero so tests need not sleep.
type harness struct {
	t    *testing.T
	gh   *vcstest.FakeGitHub
	st   *store.Store
	db   *pgx.Conn
	svc  *Service
	ctx  context.Context
	cred string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	gh := vcstest.NewFakeGitHub(t)
	t.Setenv("FORGE_SECRET_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, baseDSN())
	if err != nil {
		t.Skipf("postgres not reachable (%v); skipping DB-backed review test", err)
	}
	name := fmt.Sprintf("forge_review_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		admin.Close(ctx)
		t.Skipf("cannot create throwaway db (%v); skipping", err)
	}
	u, _ := url.Parse(baseDSN())
	u.Path = "/" + name
	st, err := store.New(ctx, u.String())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	db, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Close(ctx)
		st.Close()
		_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		admin.Close(ctx)
	})
	repo := gh.Repo()
	if err := st.RegisterRepo(ctx, proto.RepoRegistration{Repo: repo.Repo, Provider: "github",
		CloneURL: repo.CloneURL, Token: repo.Token, DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	old := debounce
	debounce = 0
	t.Cleanup(func() { debounce = old })
	_, cred, _, err := st.ReviewTarget(ctx, repo.Repo)
	if err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, gh: gh, st: st, db: db, svc: New(st), ctx: ctx, cred: cred}
}

func (h *harness) ingest(id, kind string) int {
	h.t.Helper()
	hdr, body, _ := h.gh.Delivery(h.t, id, kind)
	n, err := Ingest(h.ctx, h.st, "github", hdr, body)
	if err != nil {
		h.t.Fatal(err)
	}
	return n
}

func (h *harness) open() []store.ChangeSummary {
	h.t.Helper()
	list, _, err := h.st.ListChanges(h.ctx, store.ChangeQuery{})
	if err != nil {
		h.t.Fatal(err)
	}
	return list
}

func (h *harness) calls(class, result string) int64 {
	stats, _ := h.st.ProviderCallsSince(h.ctx, time.Now().Add(-time.Hour))
	var n int64
	for _, c := range stats {
		if c.Class == class && c.Result == result {
			n += c.Calls
		}
	}
	return n
}

func (h *harness) reconcileNow() {
	h.t.Helper()
	if _, err := h.db.Exec(h.ctx, `UPDATE change_sync SET next_reconcile_at = now()`); err != nil {
		h.t.Fatal(err)
	}
	h.svc.ReconcileAll(h.ctx)
}

// TestWebhookBurstIsOneRefresh: ingest makes no provider call and the page
// updates from the payload at once; a burst of deliveries for one change
// costs exactly one GetChange.
func TestWebhookBurstIsOneRefresh(t *testing.T) {
	h := newHarness(t)
	id, _ := h.gh.Open(t, "Seat-level lock")
	h.gh.Review(t, id, "ashwin", "approve")
	before := h.gh.Requests()
	for i := 0; i < 3; i++ {
		if h.ingest(id, "change") != 1 || h.ingest(id, "review") != 1 {
			t.Fatal("delivery not ingested")
		}
	}
	if got := h.gh.Requests(); got != before {
		t.Fatalf("ingest called the host %d times", got-before)
	}
	list := h.open()
	if len(list) != 1 || list[0].Title != "Seat-level lock" || list[0].Approvals != 0 {
		t.Fatalf("inbox after webhook = %+v", list)
	}
	if n := h.svc.RefreshDue(h.ctx); n != 1 {
		t.Fatalf("refreshes claimed = %d, want 1 for the whole burst", n)
	}
	if got := h.gh.Requests() - before; got != 1 {
		t.Fatalf("provider calls for the burst = %d, want 1", got)
	}
	if list := h.open(); list[0].Approvals != 1 {
		t.Errorf("approval from refresh missing: %+v", list[0])
	}
	if n := h.svc.RefreshDue(h.ctx); n != 0 {
		t.Errorf("a second refresh ran (%d)", n)
	}
	if h.calls(vcs.ClassWebhook, "ok") != 1 {
		t.Errorf("webhook call not counted")
	}
	var remaining float64
	_ = h.db.QueryRow(h.ctx, `SELECT tokens FROM provider_budgets WHERE credential = $1`, h.cred).Scan(&remaining)
	if remaining != 4999 {
		t.Errorf("budget = %v, want the host's X-RateLimit-Remaining (4999)", remaining)
	}
}

func TestIngestIgnoresUnknownRepoAndOtherEvents(t *testing.T) {
	h := newHarness(t)
	id, _ := h.gh.Open(t, "x")
	if _, err := h.db.Exec(h.ctx, `DELETE FROM repo_registry`); err != nil {
		t.Fatal(err)
	}
	if n := h.ingest(id, "change"); n != 0 {
		t.Errorf("unregistered repo ingested %d events", n)
	}
	hdr, body, _ := h.gh.Unrelated(t)
	if n, err := Ingest(h.ctx, h.st, "github", hdr, body); n != 0 || err != nil {
		t.Errorf("push ingested: %d %v", n, err)
	}
	if n, err := Ingest(h.ctx, h.st, "bitbucket", http.Header{}, []byte(`{}`)); n != 0 || err != nil {
		t.Errorf("provider without a change source ingested: %d %v", n, err)
	}
}

// TestReconcileIsConditionalAndRepairsMissedWebhooks: the first reconcile
// backfills, a quiet repo costs one 304 and no refresh, and a change closed
// while its webhook was lost is repaired.
func TestReconcileIsConditionalAndRepairsMissedWebhooks(t *testing.T) {
	h := newHarness(t)
	a, _ := h.gh.Open(t, "A")
	h.gh.Open(t, "B")
	h.svc.ReconcileAll(h.ctx)
	if h.calls(vcs.ClassBackfill, "ok") != 1 {
		t.Fatal("first reconcile should be one backfill list call")
	}
	if n := h.svc.RefreshDue(h.ctx); n != 2 {
		t.Fatalf("backfill refreshes = %d, want 2", n)
	}
	if len(h.open()) != 2 {
		t.Fatalf("inbox = %+v", h.open())
	}
	var next time.Time
	_ = h.db.QueryRow(h.ctx, `SELECT next_reconcile_at FROM change_sync`).Scan(&next)
	if d := time.Until(next); d < 14*time.Minute || d > 16*time.Minute {
		t.Errorf("next reconcile in %s, want ~15m while changes are open", d)
	}

	before := h.gh.Requests()
	h.reconcileNow()
	if h.gh.Requests()-before != 1 || h.calls(vcs.ClassReconcile, "not_modified") != 1 {
		t.Fatalf("quiet repo: %d calls, %d not_modified", h.gh.Requests()-before, h.calls(vcs.ClassReconcile, "not_modified"))
	}
	if n, _, _ := h.st.RefreshBacklog(h.ctx); n != 0 {
		t.Errorf("a 304 reconcile queued %d refreshes", n)
	}

	h.gh.Close(t, a) // its webhook never arrives
	h.reconcileNow()
	if n := h.svc.RefreshDue(h.ctx); n != 1 {
		t.Fatalf("repair refreshes = %d, want 1", n)
	}
	if list := h.open(); len(list) != 1 || list[0].Title != "B" {
		t.Errorf("inbox after repair = %+v", list)
	}
}

// TestBudgetDefersLowPriorityWork: with 20% of the quota left, reconcile
// waits while webhook refreshes still go.
func TestBudgetDefersLowPriorityWork(t *testing.T) {
	h := newHarness(t)
	id, _ := h.gh.Open(t, "Budgeted")
	h.gh.RateLimit(1000, 5000, time.Now().Add(30*time.Minute))
	if err := h.st.ObserveProviderRate(h.ctx, h.cred, "github",
		vcs.RateInfo{Remaining: 1000, Limit: 5000, Reset: time.Now().Add(30 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	before := h.gh.Requests()
	h.svc.ReconcileAll(h.ctx)
	if h.gh.Requests() != before {
		t.Fatal("backfill ran on a low budget")
	}
	var result string
	_ = h.db.QueryRow(h.ctx, `SELECT last_result FROM change_sync`).Scan(&result)
	if result != "waiting for provider API budget" {
		t.Errorf("reconcile result = %q", result)
	}
	h.ingest(id, "review")
	if n := h.svc.RefreshDue(h.ctx); n != 1 || h.gh.Requests() != before+1 {
		t.Errorf("webhook refresh blocked on a budget it may use (%d claimed)", n)
	}
}

// TestRateLimitedRefreshWaits: a 429 empties the bucket until Retry-After and
// reschedules the refresh then, keeping the reason for the page.
func TestRateLimitedRefreshWaits(t *testing.T) {
	h := newHarness(t)
	id, _ := h.gh.Open(t, "Throttled")
	h.ingest(id, "change")
	h.gh.RejectNext(http.StatusTooManyRequests, 2*time.Minute)
	if n := h.svc.RefreshDue(h.ctx); n != 1 {
		t.Fatal("refresh not attempted")
	}
	var errText string
	var due time.Time
	_ = h.db.QueryRow(h.ctx, `SELECT refresh_error, refresh_due_at FROM changes`).Scan(&errText, &due)
	if errText == "" || time.Until(due) < 90*time.Second {
		t.Errorf("after 429: error=%q due in %s", errText, time.Until(due))
	}
	if ok, _, _ := h.st.AcquireProviderBudget(h.ctx, h.cred, "github", vcs.ClassWebhook); ok {
		t.Error("budget not exhausted after a 429")
	}
	if n := h.svc.RefreshDue(h.ctx); n != 0 {
		t.Errorf("throttled refresh retried before Retry-After (%d)", n)
	}
	if h.calls(vcs.ClassWebhook, "rate_limited") != 1 {
		t.Error("rate-limited call not counted")
	}
}

// TestPermanentErrorStopsRetrying: a revoked token (401) is not retried on a
// timer — the error stays on the page and the next webhook tries again.
func TestPermanentErrorStopsRetrying(t *testing.T) {
	h := newHarness(t)
	id, _ := h.gh.Open(t, "Revoked")
	h.ingest(id, "change")
	h.gh.RejectNext(http.StatusUnauthorized, 0)
	if n := h.svc.RefreshDue(h.ctx); n != 1 {
		t.Fatal("refresh not attempted")
	}
	var errText string
	var due *time.Time
	_ = h.db.QueryRow(h.ctx, `SELECT refresh_error, refresh_due_at FROM changes`).Scan(&errText, &due)
	if errText == "" || due != nil {
		t.Fatalf("after 401: error=%q due=%v, want error kept and no retry", errText, due)
	}
	h.ingest(id, "review")
	if n := h.svc.RefreshDue(h.ctx); n != 1 {
		t.Errorf("a new webhook did not retry the refresh (%d)", n)
	}
}

func TestBackoffGivesUp(t *testing.T) {
	if d := time.Until(backoff(store.RefreshTask{Attempts: 0})); d < 9*time.Second || d > 11*time.Second {
		t.Errorf("first backoff = %s, want ~10s", d)
	}
	if d := time.Until(backoff(store.RefreshTask{Attempts: 6})); d > 30*time.Minute+time.Second {
		t.Errorf("backoff not capped: %s", d)
	}
	if !backoff(store.RefreshTask{Attempts: maxAttempts - 1}).IsZero() {
		t.Error("backoff never gives up")
	}
}
