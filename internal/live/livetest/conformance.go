// Package livetest is the Forge Live provider conformance suite: one set of
// tests every provider must pass, run against a fake of its platform. A
// provider is "done" when Run is green for it — that is what makes the Live
// view identical whatever runs the application. The suite branches on the
// provider's declared capabilities, never on its name.
package livetest

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/live"
)

// Env is a fake platform the suite drives.
type Env interface {
	// Provider returns the provider under test, pointed at the fake.
	Provider(t *testing.T) live.Provider
	// Deploy creates an application whose health is health (Healthy,
	// Progressing or Degraded) and returns its key. A Degraded app comes
	// with a Warning event when the provider has events.
	Deploy(t *testing.T, name, health string) string
	// Change makes one visible change to one resource of the app.
	Change(t *testing.T, key string)
	// Remove deletes the application's resources.
	Remove(t *testing.T, key string)
}

// Harness names the provider under test and builds a fresh fake per test.
type Harness struct {
	Provider string
	New      func(t *testing.T) Env
}

// wait is how long a provider may take to report something (a watch is
// immediate; a fake reconcile runs every few milliseconds).
const wait = 10 * time.Second

// watch runs the provider into a tracker until the test ends.
func watch(t *testing.T, p live.Provider) (*live.Tracker, context.CancelFunc) {
	t.Helper()
	tr := live.NewTracker(live.DefaultWindow)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := p.Watch(ctx, tr); err != nil {
			t.Errorf("watch: %v", err)
		}
	}()
	stop := func() {
		cancel()
		<-done
	}
	t.Cleanup(stop)
	return tr, stop
}

// takeUntil takes deltas from tr until ok says they are what the test wants.
func takeUntil(t *testing.T, tr *live.Tracker, what string, ok func(all []live.Delta, done bool) bool) []live.Delta {
	t.Helper()
	var all []live.Delta
	synced := false
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		d, done := tr.Take(1 << 30)
		all = append(all, d...)
		synced = synced || done
		if ok(all, synced) {
			return all
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s; got %d deltas", what, len(all))
	return nil
}

func byApp(ds []live.Delta) map[string][]live.Delta {
	out := map[string][]live.Delta{}
	for _, d := range ds {
		out[d.App.Key] = append(out[d.App.Key], d)
	}
	return out
}

// Run runs the conformance suite for one provider.
func Run(t *testing.T, h Harness) {
	t.Run("capabilities", func(t *testing.T) {
		p := h.New(t).Provider(t)
		if p.Name() != h.Provider {
			t.Errorf("Name() = %q, want %q", p.Name(), h.Provider)
		}
		if c := p.Capabilities(); !c.Watch && c.ReconcileSeconds <= 0 {
			t.Errorf("a provider without watch must say how often it reconciles: %+v", c)
		}
	})

	t.Run("initial report", func(t *testing.T) {
		env := h.New(t)
		want := map[string]string{
			env.Deploy(t, "cart", live.Healthy):       live.Healthy,
			env.Deploy(t, "fraud", live.Degraded):     live.Degraded,
			env.Deploy(t, "search", live.Progressing): live.Progressing,
		}
		p := env.Provider(t)
		caps := p.Capabilities()
		tr, _ := watch(t, p)
		all := takeUntil(t, tr, "the initial report", func(_ []live.Delta, done bool) bool { return done })
		apps := byApp(all)
		for key, health := range want {
			ds := apps[key]
			if len(ds) == 0 {
				t.Errorf("app %s not reported (got %v)", key, keys(apps))
				continue
			}
			last := ds[len(ds)-1]
			if last.Status.Health.Status != health {
				t.Errorf("app %s health = %+v, want %s", key, last.Status.Health, health)
			}
			if health != live.Healthy && last.Status.Health.Message == "" {
				t.Errorf("app %s is %s without saying why", key, health)
			}
			checkResources(t, key, ds, caps)
			if caps.Events && health == live.Degraded {
				if !hasWarning(ds) {
					takeUntil(t, tr, "a warning event for "+key, func(more []live.Delta, _ bool) bool { return hasWarning(byApp(more)[key]) })
				}
			}
		}
	})

	t.Run("compact deltas", func(t *testing.T) {
		env := h.New(t)
		key := env.Deploy(t, "cart", live.Healthy)
		other := env.Deploy(t, "orders", live.Healthy)
		tr, _ := watch(t, env.Provider(t))
		initial := takeUntil(t, tr, "the initial report", func(_ []live.Delta, done bool) bool { return done })
		total := 0
		for _, d := range byApp(initial)[key] {
			total += len(d.Upserts)
		}
		env.Change(t, key)
		ds := takeUntil(t, tr, "the change", func(ds []live.Delta, _ bool) bool { return len(byApp(ds)[key]) > 0 })
		changed := 0
		for _, d := range ds {
			if d.App.Key == other {
				t.Errorf("an untouched app was resent: %+v", d)
			}
			changed += len(d.Upserts)
		}
		if changed == 0 || (total > 2 && changed >= total) {
			t.Errorf("one change resent %d of the app's %d resources; deltas must carry what changed", changed, total)
		}
	})

	t.Run("removal", func(t *testing.T) {
		env := h.New(t)
		key := env.Deploy(t, "cart", live.Healthy)
		tr, _ := watch(t, env.Provider(t))
		takeUntil(t, tr, "the initial report", func(_ []live.Delta, done bool) bool { return done })
		env.Remove(t, key)
		takeUntil(t, tr, "the app to be gone", func(ds []live.Delta, _ bool) bool {
			for _, d := range ds {
				if d.App.Key == key && d.Gone {
					return true
				}
			}
			return false
		})
	})

	t.Run("fresh report is the same", func(t *testing.T) {
		env := h.New(t)
		key := env.Deploy(t, "cart", live.Healthy)
		uids := func() map[string]bool {
			tr, stop := watch(t, env.Provider(t))
			defer stop()
			out := map[string]bool{}
			for _, d := range takeUntil(t, tr, "a report", func(_ []live.Delta, done bool) bool { return done }) {
				for _, r := range d.Upserts {
					if d.App.Key == key {
						out[r.UID] = true
					}
				}
			}
			return out
		}
		a, b := uids(), uids()
		if len(a) == 0 || len(a) != len(b) {
			t.Fatalf("reports differ: %d vs %d resources", len(a), len(b))
		}
		for uid := range a {
			if !b[uid] {
				t.Errorf("resource %s has no stable identity across reports", uid)
			}
		}
	})
}

// checkResources: every resource is identified, its health is a known
// status, its parents are in the same app, and its manifest is there exactly
// when the provider says it has manifests.
func checkResources(t *testing.T, key string, ds []live.Delta, caps live.Capabilities) {
	t.Helper()
	uids := map[string]bool{}
	var all []live.Resource
	for _, d := range ds {
		for _, r := range d.Upserts {
			uids[r.UID] = true
			all = append(all, r)
		}
	}
	if len(all) == 0 {
		t.Errorf("app %s: no resources", key)
	}
	roots := 0
	for _, r := range all {
		if r.Kind == "" || r.Name == "" || r.UID == "" {
			t.Errorf("app %s: unidentified resource %+v", key, r)
		}
		if r.Health != nil && !live.ValidHealth(r.Health.Status) {
			t.Errorf("app %s: %s %s health %q", key, r.Kind, r.Name, r.Health.Status)
		}
		if len(r.Parents) == 0 {
			roots++
		}
		for _, p := range r.Parents {
			if !uids[p] {
				t.Errorf("app %s: %s %s has a parent outside the app (%s)", key, r.Kind, r.Name, p)
			}
		}
		if caps.Manifests != (len(r.Manifest) > 0) {
			t.Errorf("app %s: %s %s manifest present = %v, capabilities say %v", key, r.Kind, r.Name, len(r.Manifest) > 0, caps.Manifests)
		}
		if len(r.Manifest) > 0 && !json.Valid(r.Manifest) {
			t.Errorf("app %s: %s %s manifest is not JSON", key, r.Kind, r.Name)
		}
	}
	if roots == 0 && len(all) > 0 {
		t.Errorf("app %s: no top-level resource", key)
	}
}

func hasWarning(ds []live.Delta) bool {
	for _, d := range ds {
		for _, e := range d.Events {
			if e.Type == "Warning" {
				return true
			}
		}
	}
	return false
}

func keys(m map[string][]live.Delta) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
