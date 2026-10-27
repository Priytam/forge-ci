package live

import (
	"context"
	"errors"
	"hash/fnv"
	"iter"
	"reflect"
	"sort"
	"sync"
	"time"
)

// Sink receives a provider's report: every resource once, then each change.
type Sink interface {
	Upsert(app App, r Resource)
	Delete(app App, uid string)
	Event(app App, e Event)
	// Desired records the app's desired state (providers with Diff), and
	// SetSync a resource's comparison with it.
	Desired(app App, d Desired)
	// prune: Forge applied the resource and Git no longer has it.
	SetSync(app App, uid, sync string, prune bool)
	// Synced: the initial report is complete.
	Synced()
}

// Operator is implemented by providers that act on what they watch: sync,
// rollback, refresh, logs. out receives a logs operation's output as it
// arrives; Operate returns when the operation is over.
type Operator interface {
	Operate(ctx context.Context, op Op, out func([]byte)) OpResult
}

// Provider is one platform Forge Live can watch. Implementations run in
// forge-agent, next to their credentials; the server never calls them.
type Provider interface {
	Name() string
	Capabilities() Capabilities
	// Watch reports the target to sink until ctx ends, calling Synced once
	// the initial state is complete. A fresh Watch reports everything again.
	Watch(ctx context.Context, sink Sink) error
}

// ErrRestart: the provider needs to report everything afresh (a new epoch).
var ErrRestart = errors.New("live: provider asked for a fresh report")

// DefaultWindow is the shortest time an application's changes wait for more
// before being sent: a rollout touching a Deployment, its ReplicaSets and
// every pod costs one delta per window, not one per object.
const DefaultWindow = 250 * time.Millisecond

// DefaultMaxPending bounds the bytes of changes waiting to be sent. When
// Forge is slow or away, the provider blocks at that point instead of the
// agent growing: Kubernetes watches pause (and relist when the API server
// gives up on them), lists page more slowly. This is the agent's backpressure.
const DefaultMaxPending = 16 << 20

// maxPendingEvents bounds the events kept per app between two flushes.
const maxPendingEvents = 50

// Tracker is the agent's memory: each application's resources in compact form
// (no manifests) and the changes not yet sent, coalesced per resource — a
// pod that changes ten times inside a window is sent once, in its last state.
type Tracker struct {
	window     time.Duration
	maxPending int
	now        func() time.Time

	mu      sync.Mutex
	drained sync.Cond // signalled when pending shrinks
	apps    map[string]*tracked
	objects int
	pending int  // bytes of unsent upserts
	synced  bool // the provider finished its initial report
	initial bool // ...and not every app has had its Keep yet
	closed  bool
}

type entry struct {
	res Resource // without its manifest
	sum uint64   // the manifest's hash, to drop no-op updates
}

type tracked struct {
	app       App
	resources map[string]entry
	keep      bool // owes the server its full uid list (once per epoch)
	// Pending changes, since dirty (zero when nothing is pending).
	dirty   time.Time
	upserts map[string]Resource
	deletes map[string]bool
	events  []Event
	desired *Desired
}

// NewTracker returns an empty tracker that coalesces for window (at least
// DefaultWindow).
func NewTracker(window time.Duration) *Tracker {
	if window < DefaultWindow {
		window = DefaultWindow
	}
	t := &Tracker{window: window, maxPending: DefaultMaxPending, now: time.Now, apps: map[string]*tracked{}}
	t.drained.L = &t.mu
	return t
}

func (t *Tracker) get(app App) *tracked {
	a := t.apps[app.Key]
	if a == nil {
		a = &tracked{resources: map[string]entry{}, keep: t.initial}
		t.apps[app.Key] = a
	}
	a.app = app
	return a
}

func (a *tracked) touch(now time.Time) {
	if a.dirty.IsZero() {
		a.dirty = now
	}
	if a.upserts == nil {
		a.upserts, a.deletes = map[string]Resource{}, map[string]bool{}
	}
}

// resourceCost estimates a resource's encoded size, to fill batches without
// encoding twice.
func resourceCost(r Resource) int { return 320 + len(r.Manifest) }

// Upsert records r's current state. An update that changes nothing — same
// summary, same manifest — is dropped. It blocks while too much is waiting
// to be sent.
func (t *Tracker) Upsert(app App, r Resource) {
	sum := manifestSum(r.Manifest)
	stored := r
	stored.Manifest = nil
	t.mu.Lock()
	defer t.mu.Unlock()
	for t.pending >= t.maxPending && !t.closed {
		t.drained.Wait()
	}
	if t.closed {
		return
	}
	a := t.get(app)
	old, had := a.resources[r.UID]
	if had && old.sum == sum && reflect.DeepEqual(old.res, stored) {
		return
	}
	if !had {
		t.objects++
	}
	a.resources[r.UID] = entry{res: stored, sum: sum}
	a.touch(t.now())
	if prev, ok := a.upserts[r.UID]; ok {
		t.pending -= resourceCost(prev)
	}
	t.pending += resourceCost(r)
	a.upserts[r.UID] = r
	delete(a.deletes, r.UID)
}

// Delete records that the resource is gone.
func (t *Tracker) Delete(app App, uid string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	a := t.apps[app.Key]
	if a == nil {
		return
	}
	if _, ok := a.resources[uid]; !ok {
		return
	}
	t.objects--
	delete(a.resources, uid)
	a.touch(t.now())
	if prev, ok := a.upserts[uid]; ok {
		t.pending -= resourceCost(prev)
		delete(a.upserts, uid)
	}
	a.deletes[uid] = true
}

// Event records an event about one of app's resources.
func (t *Tracker) Event(app App, e Event) {
	t.mu.Lock()
	defer t.mu.Unlock()
	a := t.apps[app.Key]
	if a == nil {
		return // events about apps the tracker does not know are noise
	}
	a.touch(t.now())
	if len(a.events) >= maxPendingEvents {
		a.events = a.events[1:]
	}
	a.events = append(a.events, e)
}

// Desired records the app's desired state; the latest one is sent.
func (t *Tracker) Desired(app App, d Desired) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	a := t.get(app)
	a.touch(t.now())
	a.desired = &d
}

// SetSync records a resource's sync status (its other fields unchanged).
func (t *Tracker) SetSync(app App, uid, sync string, prune bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	a := t.apps[app.Key]
	if a == nil || t.closed {
		return
	}
	e, ok := a.resources[uid]
	if !ok || (e.res.Sync == sync && e.res.RequiresPruning == prune) {
		return
	}
	e.res.Sync, e.res.RequiresPruning = sync, prune
	a.resources[uid] = e
	a.touch(t.now())
	if up, ok := a.upserts[uid]; ok {
		up.Sync, up.RequiresPruning = sync, prune
		a.upserts[uid] = up
		return
	}
	t.pending += resourceCost(e.res)
	a.upserts[uid] = e.res
}

// Synced marks the provider's initial report complete: every app now owes
// the server its full uid list.
func (t *Tracker) Synced() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.synced {
		return
	}
	t.synced, t.initial = true, true
	for _, a := range t.apps {
		a.keep = true
		a.touch(t.now())
	}
}

// Close ends the tracker's epoch: a provider blocked on backpressure is
// released, and everything it reports from now on is dropped.
func (t *Tracker) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	t.drained.Broadcast()
}

// Counts returns the resources and applications tracked.
func (t *Tracker) Counts() (objects, apps int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.objects, len(t.apps)
}

func (a *tracked) all() iter.Seq[Resource] {
	return func(yield func(Resource) bool) {
		for _, e := range a.resources {
			if !yield(e.res) {
				return
			}
		}
	}
}

// Take returns the deltas ready to send — apps whose first unsent change is
// at least a window old, and every app that owes its Keep — at most about
// maxBytes of them (always at least one delta when any is ready; a big app's
// upserts are split across takes). done is true exactly once: on the take
// that hands out the last Keep of the initial report.
func (t *Tracker) Take(maxBytes int) (deltas []Delta, done bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	defer t.drained.Broadcast()
	now := t.now()
	keys := make([]string, 0, len(t.apps))
	for k, a := range t.apps {
		if !a.dirty.IsZero() && (a.keep || now.Sub(a.dirty) >= t.window) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	budget, more := maxBytes, false
	for _, k := range keys {
		if budget <= 0 {
			more = true
			break
		}
		a := t.apps[k]
		d := Delta{App: a.app, Events: a.events, Desired: a.desired}
		a.desired = nil
		if t.synced {
			st := StatusOf(a.all())
			d.Status = &st
		}
		uids := make([]string, 0, len(a.upserts))
		for uid := range a.upserts {
			uids = append(uids, uid)
		}
		sort.Strings(uids)
		for _, uid := range uids {
			if budget <= 0 && len(d.Upserts) > 0 {
				more = true
				break
			}
			r := a.upserts[uid]
			budget -= resourceCost(r)
			t.pending -= resourceCost(r)
			d.Upserts = append(d.Upserts, r)
			delete(a.upserts, uid)
		}
		for uid := range a.deletes {
			d.Deletes = append(d.Deletes, uid)
		}
		sort.Strings(d.Deletes)
		budget -= 64*len(d.Deletes) + 256*len(d.Events)
		a.deletes, a.events = map[string]bool{}, nil
		if len(a.upserts) > 0 {
			deltas = append(deltas, d)
			continue // the rest of this app goes in the next take
		}
		a.dirty = time.Time{}
		if a.keep {
			a.keep = false
			d.Keep = make([]string, 0, len(a.resources))
			for uid := range a.resources {
				d.Keep = append(d.Keep, uid)
			}
			sort.Strings(d.Keep)
			budget -= 40 * len(d.Keep)
		}
		if len(a.resources) == 0 {
			d.Gone = true
			delete(t.apps, k)
		}
		deltas = append(deltas, d)
	}
	if t.initial && !more {
		for _, a := range t.apps {
			if a.keep {
				return deltas, false
			}
		}
		t.initial, done = false, true
	}
	return deltas, done
}

func manifestSum(b []byte) uint64 {
	h := fnv.New64a()
	_, _ = h.Write(b)
	return h.Sum64()
}
