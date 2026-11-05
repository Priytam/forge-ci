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

// Sync sets the deployed image (a new revision takes the traffic), a dry
// run validates only, rollback sends all traffic back to a revision.
func TestOperate(t *testing.T) {
	f := livetest.NewFakeCloudRun(t)
	key := f.Deploy(t, "cart", live.Healthy)
	p, err := cloudrun.New(cloudrun.Config{Project: "acme-prod", Regions: []string{"asia-south1"}, BaseURL: f.Server.URL,
		Period: time.Hour, CallsPerMinute: 6000, AllowSync: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	app := live.App{Key: key}
	if res := p.Operate(ctx, live.Op{Kind: live.OpSync, App: app, Image: "gcr.io/acme/cart@sha256:beef", DryRun: true}, nil); !res.OK ||
		res.Resources[0].Action != "would-configure" {
		t.Errorf("dry run = %+v", res)
	}
	if res := p.Operate(ctx, live.Op{Kind: live.OpSync, App: app, Image: "gcr.io/acme/cart@sha256:beef"}, nil); !res.OK ||
		res.Resources[0].Action != "configured" {
		t.Errorf("sync = %+v", res)
	}
	if res := p.Operate(ctx, live.Op{Kind: live.OpSync, App: app, Image: "gcr.io/acme/cart@sha256:beef"}, nil); res.Resources[0].Action != "unchanged" {
		t.Errorf("a second sync changed something: %+v", res)
	}
	if res := p.Operate(ctx, live.Op{Kind: live.OpRollback, App: app, Revision: "cart-00001"}, nil); !res.OK {
		t.Errorf("rollback = %+v", res)
	}
	if f.Patches != 3 {
		t.Errorf("%d patches, want 3 (dry run, sync, rollback)", f.Patches)
	}
	ro, _ := cloudrun.New(cloudrun.Config{Project: "acme-prod", Regions: []string{"asia-south1"}, BaseURL: f.Server.URL})
	if res := ro.Operate(ctx, live.Op{Kind: live.OpSync, App: app, Image: "x"}, nil); res.OK || ro.Capabilities().Sync {
		t.Errorf("a read-only agent synced: %+v", res)
	}
}
