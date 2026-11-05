package live

import (
	"maps"
	"strings"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }
func newTestTracker() (*Tracker, *clock) {
	c := &clock{t: time.Unix(1_790_000_000, 0)}
	t := NewTracker(DefaultWindow)
	t.now = c.now
	return t, c
}

var cart = App{Key: "checkout/cart-api", Name: "cart-api", Namespace: "checkout"}

func pod(uid, status string, restarts int) Resource {
	return Resource{Kind: "Pod", Name: "cart-" + uid, UID: uid, Parents: []string{"rs"},
		Health: &Health{Status: status}, Summary: Summary{Restarts: restarts}, Manifest: []byte(`{"restarts":` + string(rune('0'+restarts)) + `}`)}
}

func TestTrackerCoalescesWithinTheWindow(t *testing.T) {
	tr, c := newTestTracker()
	tr.Synced()
	tr.Take(1 << 20) // the (empty) initial report
	for i := 0; i < 9; i++ {
		tr.Upsert(cart, pod("p1", Progressing, i))
	}
	if ds, _ := tr.Take(1 << 20); len(ds) != 0 {
		t.Fatalf("taken before the window: %+v", ds)
	}
	c.advance(DefaultWindow)
	ds, _ := tr.Take(1 << 20)
	if len(ds) != 1 || len(ds[0].Upserts) != 1 || ds[0].Upserts[0].Summary.Restarts != 8 {
		t.Fatalf("want one upsert in its last state, got %+v", ds)
	}
	// The same state again is not a change.
	tr.Upsert(cart, pod("p1", Progressing, 8))
	c.advance(time.Second)
	if ds, _ := tr.Take(1 << 20); len(ds) != 0 {
		t.Errorf("a no-op update was sent: %+v", ds)
	}
}

func TestTrackerInitialReportStreamsThenKeeps(t *testing.T) {
	tr, c := newTestTracker()
	orders := App{Key: "checkout/orders", Name: "orders"}
	tr.Upsert(cart, pod("p1", Healthy, 0))
	tr.Upsert(orders, pod("o1", Degraded, 3))
	c.advance(DefaultWindow)
	ds, done := tr.Take(1 << 20)
	if done || len(ds) != 2 || ds[0].Status != nil || ds[0].Keep != nil {
		t.Fatalf("before Synced: deltas stream without status or keep, got %+v done=%v", ds, done)
	}
	tr.Upsert(cart, pod("p2", Healthy, 0))
	tr.Synced()
	ds, done = tr.Take(1 << 20)
	if !done || len(ds) != 2 {
		t.Fatalf("after Synced: every app sends its keep, got %+v done=%v", ds, done)
	}
	if ds[0].App.Key != cart.Key || strings.Join(ds[0].Keep, ",") != "p1,p2" || len(ds[0].Upserts) != 1 || ds[0].Status.Resources != 2 {
		t.Errorf("cart delta = %+v", ds[0])
	}
	if ds[1].Status.Health.Status != Degraded || ds[1].Status.Health.Message != "Pod cart-o1" {
		t.Errorf("orders status = %+v", ds[1].Status)
	}
	if _, done := tr.Take(1 << 20); done {
		t.Error("done must be reported once")
	}
}

func TestTrackerSplitsBigAppsAcrossBatches(t *testing.T) {
	tr, _ := newTestTracker()
	for i := 0; i < 10; i++ {
		r := pod(string(rune('a'+i)), Healthy, 0)
		r.Manifest = []byte(`"` + strings.Repeat("x", 1000) + `"`)
		tr.Upsert(cart, r)
	}
	tr.Synced()
	var got []Delta
	for i := 0; ; i++ {
		ds, done := tr.Take(3000)
		got = append(got, ds...)
		if done {
			break
		}
		if i > 10 {
			t.Fatal("never done")
		}
	}
	upserts := 0
	for i, d := range got {
		upserts += len(d.Upserts)
		if (d.Keep != nil) != (i == len(got)-1) {
			t.Errorf("keep must come with the app's last chunk only (chunk %d of %d)", i+1, len(got))
		}
	}
	if len(got) < 3 || upserts != 10 {
		t.Errorf("%d chunks carrying %d upserts", len(got), upserts)
	}
}

func TestTrackerBackpressure(t *testing.T) {
	tr, _ := newTestTracker()
	tr.maxPending = 1000
	tr.Synced()
	tr.Upsert(cart, Resource{Kind: "Pod", Name: "a", UID: "a", Manifest: []byte(strings.Repeat("x", 900))})
	blocked := make(chan struct{})
	go func() {
		tr.Upsert(cart, Resource{Kind: "Pod", Name: "b", UID: "b"})
		close(blocked)
	}()
	select {
	case <-blocked:
		t.Fatal("upsert did not wait for pending changes to be sent")
	case <-time.After(50 * time.Millisecond):
	}
	tr.Take(1 << 20)
	select {
	case <-blocked:
	case <-time.After(time.Second):
		t.Fatal("upsert still blocked after the take")
	}
}

func TestTrackerAppGoneWithItsLastResource(t *testing.T) {
	tr, c := newTestTracker()
	tr.Upsert(cart, pod("p1", Healthy, 0))
	tr.Synced()
	tr.Take(1 << 20)
	tr.Delete(cart, "p1")
	c.advance(DefaultWindow)
	ds, _ := tr.Take(1 << 20)
	if len(ds) != 1 || !ds[0].Gone || len(ds[0].Deletes) != 1 {
		t.Fatalf("got %+v", ds)
	}
	if objects, apps := tr.Counts(); objects != 0 || apps != 0 {
		t.Errorf("counts = %d objects, %d apps", objects, apps)
	}
}

func TestStatusOf(t *testing.T) {
	rs := map[string]Resource{
		"d":  {Kind: "Deployment", Name: "cart", UID: "d", Health: &Health{Status: Progressing, Message: "rolling"}, Summary: Summary{Images: []string{"cart:v2"}}},
		"p1": {Kind: "Pod", Name: "cart-1", UID: "p1", Parents: []string{"d"}, Health: &Health{Status: Healthy}, Summary: Summary{Images: []string{"cart:v1"}}},
		"p2": {Kind: "Pod", Name: "cart-2", UID: "p2", Parents: []string{"d"}, Health: &Health{Status: Degraded, Message: "app: CrashLoopBackOff"}},
		"cm": {Kind: "ConfigMap", Name: "cart-config", UID: "cm"},
	}
	st := StatusOf(maps.Values(rs))
	if st.Health.Status != Degraded || st.Health.Message != "Pod cart-2: app: CrashLoopBackOff" {
		t.Errorf("health = %+v", st.Health)
	}
	if st.Resources != 4 || st.Pods != 2 || st.PodsReady != 1 || len(st.Images) != 1 || st.Images[0] != "cart:v2" {
		t.Errorf("status = %+v", st)
	}
	// A tie names the most specific resource: the pod says why.
	rs["d"] = Resource{Kind: "Deployment", Name: "cart", UID: "d", Health: &Health{Status: Degraded, Message: "exceeded its progress deadline"}}
	if st := StatusOf(maps.Values(rs)); st.Health.Message != "Pod cart-2: app: CrashLoopBackOff" {
		t.Errorf("tie message = %q", st.Health.Message)
	}
	if st := StatusOf(maps.Values(map[string]Resource{"cm": rs["cm"]})); st.Health.Status != Healthy {
		t.Errorf("an app of kinds without health is Healthy, got %+v", st.Health)
	}
}
