package cloudrun_test

import (
	"context"
	"testing"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/live"
	"github.com/priytamjeepandey/forge-ci/internal/live/cloudrun"
	"github.com/priytamjeepandey/forge-ci/internal/live/livetest"
)

// run watches f for d and returns how many API calls that cost.
func run(t *testing.T, f *livetest.FakeCloudRun, cfg cloudrun.Config, d time.Duration) int {
	t.Helper()
	cfg.Project, cfg.Regions, cfg.BaseURL = "acme-prod", []string{"asia-south1"}, f.Server.URL
	p, err := cloudrun.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	before := f.Calls()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	_ = p.Watch(ctx, live.NewTracker(live.DefaultWindow))
	// The provider counts attempts; one may be cut off by the deadline.
	if got, saw := int(p.Calls()), f.Calls()-before; got < saw || got > saw+1 {
		t.Errorf("provider counted %d calls, the API saw %d", got, saw)
	}
	return f.Calls() - before
}

// An unchanged fleet costs one list per region per reconcile: revisions are
// fetched only for services whose fingerprint changed.
func TestReconcileIsConditional(t *testing.T) {
	f := livetest.NewFakeCloudRun(t)
	for _, n := range []string{"cart", "orders", "payments"} {
		f.Deploy(t, n, live.Healthy)
	}
	calls := run(t, f, cloudrun.Config{Period: 50 * time.Millisecond, CallsPerMinute: 60_000}, 525*time.Millisecond)
	// ~11 reconciles: 11 lists + 3 revision lists on the first.
	if calls < 10 || calls > 16 {
		t.Errorf("%d calls for ~11 reconciles of 3 unchanged services, want ≈ 14", calls)
	}
}

// The budget holds whatever the period: 120 calls per minute is 2 per second.
func TestCallBudget(t *testing.T) {
	f := livetest.NewFakeCloudRun(t)
	f.Deploy(t, "cart", live.Healthy)
	calls := run(t, f, cloudrun.Config{Period: time.Millisecond, CallsPerMinute: 120}, 2*time.Second)
	// A full bucket (120) may be spent at once, then 2 per second.
	if calls > 120+5 {
		t.Errorf("%d calls in 2s with a budget of 120/minute", calls)
	}
}
