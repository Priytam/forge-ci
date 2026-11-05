// Package hub is the server's side of Forge Live. It applies agent batches,
// keeps each application's resource tree in the blob store (one
// content-addressed blob per version) with the hot ones in memory, stores
// each resource's live manifest, and fans per-app patches out to live pages.
//
// Pages read through it from Postgres, the blob store and memory only: the
// server has no provider code at all (a test keeps it that way), so a page
// can never call a cluster or a cloud API.
package hub

import (
	"bytes"
	"compress/gzip"
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/blob"
	"github.com/priytamjeepandey/forge-ci/internal/live"
	"github.com/priytamjeepandey/forge-ci/internal/store"
)

// Tree is one version of an application's resources (manifests are stored
// apart, per resource).
type Tree struct {
	App       int64           `json:"app"`
	Version   int64           `json:"version"`
	Resources []live.Resource `json:"resources"` // sorted by uid
}

// cost estimates a tree's memory, for the LRU.
func (t *Tree) cost() int { return 256 + 512*len(t.Resources) }

// Hub is one server's Forge Live state.
type Hub struct {
	st    *store.Store
	blobs blob.Store
	trees *lru

	mu        sync.Mutex
	subs      map[*sub]struct{}
	published map[int64]*Tree // the last tree each watched app's subscribers were sent
	stop      context.CancelFunc

	ops opWaiters
	// autoWake: an ingest left an app out of sync; auto-sync looks now
	// rather than at its next tick.
	autoWake chan struct{}
}

// New returns a hub keeping up to cacheBytes of trees in memory.
func New(st *store.Store, blobs blob.Store, cacheBytes int) *Hub {
	return &Hub{st: st, blobs: blobs, trees: newLRU(cacheBytes), subs: map[*sub]struct{}{}, published: map[int64]*Tree{},
		autoWake: make(chan struct{}, 1)}
}

func treeKey(digest string) string { return "live/trees/" + digest + ".json.gz" }

func manifestKey(app int64, uid string) string {
	return fmt.Sprintf("live/manifests/%d/%s.json", app, uid)
}

// desiredKey is where a desired manifest lives; resource keys hold slashes,
// so the blob is named by their hash.
func desiredKey(app int64, key string) string {
	sum := sha256.Sum256([]byte(key))
	return fmt.Sprintf("live/desired/%d/%s.json", app, hex.EncodeToString(sum[:16]))
}

// ---- ingest ----

// Ingest applies one agent batch for target. It is idempotent per (epoch,
// seq): a retried batch is acknowledged without being applied twice, and a
// batch that does not follow the last one asks the agent for a fresh epoch.
func (h *Hub) Ingest(ctx context.Context, targetID int64, b live.Batch, size int) (live.Ack, error) {
	var ack live.Ack
	var trees []*Tree
	var gone []int64
	outOfSync := false
	err := h.st.LiveIngest(ctx, targetID, func(tx *store.LiveTx) error {
		t := tx.Target
		t.Provider, t.Capabilities, t.AgentVersion, t.Stats = b.Provider, b.Capabilities, b.Version, b.Stats
		t.BytesIn += int64(size)
		if b.Epoch != t.Epoch {
			if b.Seq != 1 {
				ack.Resync = true
				return nil
			}
			t.Epoch, t.Seq, t.Synced = b.Epoch, 0, false
		}
		switch {
		case b.Seq <= t.Seq:
			ack.Seq = b.Seq // a retry of a batch already applied
			return nil
		case b.Seq != t.Seq+1:
			ack.Resync = true
			return nil
		}
		for _, d := range b.Deltas {
			tree, removed, err := h.apply(ctx, tx, d)
			if err != nil {
				return fmt.Errorf("app %s: %w", d.App.Key, err)
			}
			if tree != nil {
				trees = append(trees, tree)
			}
			if removed != 0 {
				gone = append(gone, removed)
			}
			outOfSync = outOfSync || d.Status != nil && d.Status.Sync == live.OutOfSync && !d.App.Orphans
		}
		if b.Synced {
			stale, err := tx.StaleApps(ctx)
			if err != nil {
				return err
			}
			for _, a := range stale {
				if err := h.remove(ctx, tx, &a); err != nil {
					return err
				}
				gone = append(gone, a.ID)
			}
			t.Synced = true
		}
		t.Seq, ack.Seq = b.Seq, b.Seq
		return nil
	})
	if err != nil {
		return live.Ack{}, err
	}
	for _, t := range trees {
		h.trees.put(t)
	}
	for _, id := range gone {
		h.trees.drop(id)
	}
	if outOfSync {
		select {
		case h.autoWake <- struct{}{}:
		default:
		}
	}
	return ack, nil
}

// apply merges one delta into its app: manifests to the blob store, a new
// tree version when anything changed, the status, the events.
func (h *Hub) apply(ctx context.Context, tx *store.LiveTx, d live.Delta) (*Tree, int64, error) {
	a, err := tx.App(ctx, d.App.Key)
	if err != nil {
		return nil, 0, err
	}
	if d.Gone {
		if a == nil {
			return nil, 0, nil
		}
		return nil, a.ID, h.remove(ctx, tx, a)
	}
	now := time.Now()
	if a == nil {
		a = &store.LiveApp{Key: d.App.Key, Health: live.Unknown, HealthSince: now, Orphans: d.App.Orphans}
		a.Name, a.Namespace, a.Group, a.Epoch = d.App.Name, d.App.Namespace, d.App.Group, tx.Target.Epoch
		if err := tx.SaveApp(ctx, a, true); err != nil { // the id names its manifests
			return nil, 0, err
		}
	}
	before := *a
	old, err := h.tree(ctx, a)
	if err != nil {
		return nil, 0, err
	}
	res := make(map[string]live.Resource, len(old.Resources)+len(d.Upserts))
	for _, r := range old.Resources {
		res[r.UID] = r
	}
	var garbage []string
	for _, r := range d.Upserts {
		if len(r.Manifest) > 0 {
			if _, err := h.blobs.Put(ctx, manifestKey(a.ID, r.UID), bytes.NewReader(r.Manifest)); err != nil {
				return nil, 0, err
			}
			r.Manifest = nil
		}
		res[r.UID] = r
	}
	drop := func(uid string) {
		if _, ok := res[uid]; ok {
			delete(res, uid)
			garbage = append(garbage, manifestKey(a.ID, uid))
		}
	}
	for _, uid := range d.Deletes {
		drop(uid)
	}
	if d.Keep != nil {
		keep := make(map[string]bool, len(d.Keep))
		for _, uid := range d.Keep {
			keep[uid] = true
		}
		for uid := range res {
			if !keep[uid] {
				drop(uid)
			}
		}
	}
	var next *Tree
	if len(d.Upserts) > 0 || len(garbage) > 0 || a.Tree == "" {
		next = &Tree{App: a.ID, Version: a.Version + 1, Resources: make([]live.Resource, 0, len(res))}
		for _, r := range res {
			next.Resources = append(next.Resources, r)
		}
		slices.SortFunc(next.Resources, func(x, y live.Resource) int { return cmpString(x.UID, y.UID) })
		key, err := h.putTree(ctx, next)
		if err != nil {
			return nil, 0, err
		}
		if a.Tree != "" && a.Tree != key {
			garbage = append(garbage, a.Tree)
		}
		a.Tree, a.Version = key, next.Version
	}
	if d.Desired != nil {
		g, err := h.setDesired(ctx, a, d.Desired)
		if err != nil {
			return nil, 0, err
		}
		garbage = append(garbage, g...)
	}
	if err := tx.Collect(ctx, garbage...); err != nil {
		return nil, 0, err
	}
	if d.Status != nil {
		a.SetStatus(*d.Status, now)
	}
	if a.SourceError != "" {
		a.Sync = "Unknown"
	}
	if t := tx.Target; !t.Capabilities.Diff && t.Repo != "" && d.Status != nil {
		// No Git (Cloud Run): the desired state is what Forge last deployed.
		digest, err := h.st.LatestDeployDigest(ctx, t.Repo, t.Environment)
		if err != nil {
			return nil, 0, err
		}
		if digest != "" {
			a.Sync = live.OutOfSync
			for _, img := range a.Images {
				if strings.HasSuffix(img, "@"+digest) {
					a.Sync = live.Synced
				}
			}
		}
	}
	a.Name, a.Namespace, a.Group, a.Epoch, a.Template = d.App.Name, d.App.Namespace, d.App.Group, tx.Target.Epoch, d.App.Template
	if err := tx.UpsertEvents(ctx, a.ID, d.Events); err != nil {
		return nil, 0, err
	}
	if err := tx.SaveApp(ctx, a, summaryChanged(&before, a)); err != nil {
		return nil, 0, err
	}
	return next, 0, nil
}

func cmpString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// setDesired stores an app's desired manifests (one blob per resource key)
// and its revision; it returns the blobs of keys no longer desired.
func (h *Hub) setDesired(ctx context.Context, a *store.LiveApp, d *live.Desired) ([]string, error) {
	keys := make([]string, 0, len(d.Manifests))
	for key, m := range d.Manifests {
		if _, err := h.blobs.Put(ctx, desiredKey(a.ID, key), bytes.NewReader(m)); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	slices.Sort(keys)
	var garbage []string
	for _, k := range a.DesiredKeys {
		if _, ok := d.Manifests[k]; !ok {
			garbage = append(garbage, desiredKey(a.ID, k))
		}
	}
	a.DesiredKeys = keys
	r := d.Revision
	a.Revision, a.RevisionMessage, a.RevisionAuthor = r.SHA, r.Message, r.Author
	a.RevisionAt = nil
	if !r.Time.IsZero() {
		a.RevisionAt = &r.Time
	}
	a.SourcePath, a.SourceTool, a.SourceError, a.DesiredDigest = d.Source, d.Tool, d.Error, d.Digest
	return garbage, nil
}

// summaryChanged: what the Applications page shows of a differs.
func summaryChanged(a, b *store.LiveApp) bool {
	return a.Name != b.Name || a.Namespace != b.Namespace || a.Group != b.Group || a.Health != b.Health ||
		a.HealthMessage != b.HealthMessage || a.Resources != b.Resources || a.Pods != b.Pods ||
		a.PodsReady != b.PodsReady || !slices.Equal(a.Images, b.Images) || a.Sync != b.Sync || a.Revision != b.Revision ||
		a.SourceError != b.SourceError || a.Drifted != b.Drifted || a.Prunable != b.Prunable || a.Template != b.Template
}

// remove deletes an app and queues its blobs.
func (h *Hub) remove(ctx context.Context, tx *store.LiveTx, a *store.LiveApp) error {
	keys, err := h.blobKeys(ctx, a.ID, a.Tree, a.DesiredKeys)
	if err != nil {
		return err
	}
	if err := tx.Collect(ctx, keys...); err != nil {
		return err
	}
	return tx.DeleteApp(ctx, a.ID)
}

// blobKeys are every blob of an app: its tree, its manifests and its
// desired manifests.
func (h *Hub) blobKeys(ctx context.Context, app int64, tree string, desired []string) ([]string, error) {
	var keys []string
	for _, k := range desired {
		keys = append(keys, desiredKey(app, k))
	}
	if tree == "" {
		return keys, nil
	}
	t, err := h.loadTree(ctx, tree)
	if err != nil {
		return nil, err
	}
	keys = append(keys, tree)
	for _, r := range t.Resources {
		keys = append(keys, manifestKey(app, r.UID))
	}
	return keys, nil
}

// DeleteTarget removes a target, its apps and (after the grace period) their blobs.
func (h *Hub) DeleteTarget(ctx context.Context, id int64) error {
	trees, err := h.st.LiveTargetTrees(ctx, id)
	if err != nil {
		return err
	}
	var keys []string
	for app, tree := range trees {
		k, err := h.blobKeys(ctx, app, tree.Tree, tree.Desired)
		if err != nil {
			return err
		}
		keys = append(keys, k...)
		h.trees.drop(app)
	}
	return h.st.DeleteLiveTarget(ctx, id, keys)
}

func (h *Hub) putTree(ctx context.Context, t *Tree) (string, error) {
	raw, err := json.Marshal(t)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(raw)
	if err := zw.Close(); err != nil {
		return "", err
	}
	key := treeKey(hex.EncodeToString(sum[:]))
	_, err = h.blobs.Put(ctx, key, &buf)
	return key, err
}

func (h *Hub) loadTree(ctx context.Context, key string) (*Tree, error) {
	rc, err := h.blobs.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	zr, err := gzip.NewReader(rc)
	if err != nil {
		return nil, err
	}
	var t Tree
	if err := json.NewDecoder(zr).Decode(&t); err != nil {
		return nil, err
	}
	return &t, nil
}

// tree returns app's current tree: from memory, else its blob (an empty
// tree for an app with none yet). Callers must not modify it.
func (h *Hub) tree(ctx context.Context, a *store.LiveApp) (*Tree, error) {
	if a.Tree == "" {
		return &Tree{App: a.ID}, nil
	}
	if t := h.trees.get(a.ID); t != nil && t.Version == a.Version {
		return t, nil
	}
	t, err := h.loadTree(ctx, a.Tree)
	if err != nil {
		return nil, err
	}
	h.trees.put(t)
	return t, nil
}

// ---- reads ----

// Tree returns an application's current resource tree.
func (h *Hub) Tree(ctx context.Context, a *store.LiveApp) (*Tree, error) { return h.tree(ctx, a) }

// DesiredManifest returns a resource's desired manifest by key.
func (h *Hub) DesiredManifest(ctx context.Context, app int64, key string) ([]byte, error) {
	rc, err := h.blobs.Get(ctx, desiredKey(app, key))
	if err != nil {
		return nil, store.ErrNotFound
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// Manifest returns a resource's live manifest (store.ErrNotFound when the
// provider sends none).
func (h *Hub) Manifest(ctx context.Context, app int64, uid string) ([]byte, error) {
	rc, err := h.blobs.Get(ctx, manifestKey(app, uid))
	if err != nil {
		return nil, store.ErrNotFound
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// Sweep deletes collected blobs until ctx ends.
func (h *Hub) Sweep(ctx context.Context) {
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		keys, err := h.st.TakeLiveGarbage(ctx, 500)
		if err != nil && ctx.Err() == nil {
			slog.Warn("live: blob sweep", "err", err)
		}
		if err := h.st.ExpireLiveOps(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("live: expire operations", "err", err)
		}
		for _, k := range keys {
			if err := h.blobs.Delete(ctx, k); err != nil {
				slog.Warn("live: delete blob", "key", k, "err", err)
			}
		}
		if len(keys) == 500 {
			continue // more are due
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// ---- live pages ----

// Event is one server-sent event for a live page.
type Event struct {
	Name string // app | gone | patch
	Data []byte
}

// Patch moves a page's copy of an app's tree from Base to Version.
type Patch struct {
	App     int64           `json:"app"`
	Base    int64           `json:"base"`
	Version int64           `json:"version"`
	Row     *store.LiveApp  `json:"row"`
	Upserts []live.Resource `json:"upserts"`
	Deletes []string        `json:"deletes"`
}

type sub struct {
	app int64 // 0: summaries only
	ch  chan Event
}

// subBuffer is how many events a page may fall behind before it is cut off
// (it reconnects and refetches; a slow page never slows the others).
const subBuffer = 64

// Subscribe returns a page's event stream: every app summary change, and the
// tree patches of app (0: none). The stream is closed when the page falls
// too far behind.
func (h *Hub) Subscribe(ctx context.Context, app int64) (<-chan Event, func()) {
	s := &sub{app: app, ch: make(chan Event, subBuffer)}
	h.mu.Lock()
	h.subs[s] = struct{}{}
	if h.stop == nil {
		lctx, cancel := context.WithCancel(context.Background())
		h.stop = cancel
		go h.listen(lctx)
	}
	h.mu.Unlock()
	if app != 0 {
		h.publishBase(ctx, app)
	}
	return s.ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if _, ok := h.subs[s]; !ok {
			return // already cut off
		}
		delete(h.subs, s)
		close(s.ch)
		h.forget(app)
		if len(h.subs) == 0 && h.stop != nil {
			h.stop()
			h.stop = nil
		}
	}
}

// forget drops app's published tree once nobody watches it (h.mu held).
func (h *Hub) forget(app int64) {
	for s := range h.subs {
		if s.app == app {
			return
		}
	}
	delete(h.published, app)
}

// publishBase records the tree patches for app will start from.
func (h *Hub) publishBase(ctx context.Context, app int64) {
	h.mu.Lock()
	_, ok := h.published[app]
	h.mu.Unlock()
	if ok {
		return
	}
	a, err := h.st.LiveApp(ctx, app)
	if err != nil {
		return
	}
	t, err := h.tree(ctx, a)
	if err != nil {
		return
	}
	h.mu.Lock()
	if _, ok := h.published[app]; !ok {
		h.published[app] = t
	}
	h.mu.Unlock()
}

// listen holds the LISTEN while anyone is subscribed, reconnecting on errors.
func (h *Hub) listen(ctx context.Context) {
	for ctx.Err() == nil {
		err := h.st.ListenLive(ctx, func(payload string) { h.notified(ctx, payload) })
		if ctx.Err() == nil {
			slog.Warn("live: listener stopped, reconnecting", "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
		}
	}
}

func (h *Hub) notified(ctx context.Context, payload string) {
	var n struct {
		ID      int64 `json:"id"`
		V       int64 `json:"v"`
		Summary bool  `json:"s"`
		Gone    bool  `json:"gone"`
		Op      int64 `json:"op"`
	}
	if json.Unmarshal([]byte(payload), &n) != nil {
		return
	}
	if n.Op != 0 {
		if o, err := h.st.LiveOp(ctx, n.Op); err == nil {
			data, _ := json.Marshal(o)
			h.broadcast(func(s *sub) bool { return s.app == n.ID }, Event{Name: "op", Data: data})
		}
		return
	}
	if n.Gone {
		h.broadcast(func(*sub) bool { return true }, Event{Name: "gone", Data: []byte(fmt.Sprintf(`{"id":%d}`, n.ID))})
		return
	}
	h.mu.Lock()
	prev, watched := h.published[n.ID]
	h.mu.Unlock()
	if !n.Summary && !watched {
		return
	}
	a, err := h.st.LiveApp(ctx, n.ID)
	if err != nil {
		return
	}
	if n.Summary {
		row, _ := json.Marshal(a)
		h.broadcast(func(*sub) bool { return true }, Event{Name: "app", Data: row})
	}
	if !watched || a.Version <= prev.Version {
		return
	}
	t, err := h.tree(ctx, a)
	if err != nil {
		return
	}
	p := diff(prev, t)
	p.Row = a
	data, _ := json.Marshal(p)
	h.mu.Lock()
	if cur := h.published[n.ID]; cur == prev {
		h.published[n.ID] = t
	}
	h.mu.Unlock()
	h.broadcast(func(s *sub) bool { return s.app == n.ID }, Event{Name: "patch", Data: data})
}

// diff is the patch from tree a to tree b: changed resources and removed uids.
func diff(a, b *Tree) Patch {
	p := Patch{App: b.App, Base: a.Version, Version: b.Version, Upserts: []live.Resource{}, Deletes: []string{}}
	old := make(map[string]*live.Resource, len(a.Resources))
	for i := range a.Resources {
		old[a.Resources[i].UID] = &a.Resources[i]
	}
	for i := range b.Resources {
		r := &b.Resources[i]
		if o := old[r.UID]; o == nil || !reflect.DeepEqual(*o, *r) {
			p.Upserts = append(p.Upserts, *r)
		}
		delete(old, r.UID)
	}
	for uid := range old {
		p.Deletes = append(p.Deletes, uid)
	}
	slices.Sort(p.Deletes)
	return p
}

func (h *Hub) broadcast(to func(*sub) bool, e Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		if !to(s) {
			continue
		}
		select {
		case s.ch <- e:
		default: // too far behind: cut off; the page reconnects and refetches
			delete(h.subs, s)
			close(s.ch)
			h.forget(s.app)
		}
	}
}

// ---- tree cache ----

// lru keeps recently used trees, bounded by estimated bytes.
type lru struct {
	mu    sync.Mutex
	max   int
	size  int
	order *list.List
	items map[int64]*list.Element
}

func newLRU(max int) *lru { return &lru{max: max, order: list.New(), items: map[int64]*list.Element{}} }

func (c *lru) get(app int64) *Tree {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[app]; ok {
		c.order.MoveToFront(e)
		return e.Value.(*Tree)
	}
	return nil
}

func (c *lru) put(t *Tree) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[t.App]; ok {
		if e.Value.(*Tree).Version > t.Version {
			return
		}
		c.size -= e.Value.(*Tree).cost()
		c.order.Remove(e)
	}
	if t.cost() > c.max/4 {
		delete(c.items, t.App)
		return // one huge tree must not evict everything
	}
	c.items[t.App] = c.order.PushFront(t)
	c.size += t.cost()
	for c.size > c.max {
		last := c.order.Back()
		c.size -= last.Value.(*Tree).cost()
		delete(c.items, last.Value.(*Tree).App)
		c.order.Remove(last)
	}
}

func (c *lru) drop(app int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[app]; ok {
		c.size -= e.Value.(*Tree).cost()
		c.order.Remove(e)
		delete(c.items, app)
	}
}

// ---- operations: agents wait for them ----

// opWaiters are agents long-polling for operations, by target. One LISTEN
// connection per server runs while any agent waits.
type opWaiters struct {
	mu   sync.Mutex
	subs map[int64]map[chan struct{}]struct{}
	stop context.CancelFunc
}

// NextOp waits up to wait for an operation for target's agent and claims
// it (nil when none came).
func (h *Hub) NextOp(ctx context.Context, target int64, wait time.Duration) (*store.LiveOp, error) {
	wake, done := h.waitOps(target)
	defer done()
	deadline := time.After(wait)
	for {
		op, err := h.st.ClaimLiveOp(ctx, target)
		if err != nil || op != nil {
			return op, err
		}
		select {
		case <-ctx.Done():
			return nil, nil
		case <-deadline:
			return nil, nil
		case <-wake:
		case <-time.After(5 * time.Second): // a missed notification costs at most this
		}
	}
}

func (h *Hub) waitOps(target int64) (<-chan struct{}, func()) {
	w := &h.ops
	ch := make(chan struct{}, 1)
	w.mu.Lock()
	if w.subs == nil {
		w.subs = map[int64]map[chan struct{}]struct{}{}
	}
	if w.subs[target] == nil {
		w.subs[target] = map[chan struct{}]struct{}{}
	}
	w.subs[target][ch] = struct{}{}
	if w.stop == nil {
		ctx, cancel := context.WithCancel(context.Background())
		w.stop = cancel
		go h.listenOps(ctx)
	}
	w.mu.Unlock()
	return ch, func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		delete(w.subs[target], ch)
		if len(w.subs[target]) == 0 {
			delete(w.subs, target)
		}
		if len(w.subs) == 0 && w.stop != nil {
			w.stop()
			w.stop = nil
		}
	}
}

func (h *Hub) listenOps(ctx context.Context) {
	for ctx.Err() == nil {
		err := h.st.ListenLiveOps(ctx, func(target int64) {
			h.ops.mu.Lock()
			for ch := range h.ops.subs[target] {
				select {
				case ch <- struct{}{}:
				default:
				}
			}
			h.ops.mu.Unlock()
		})
		if ctx.Err() == nil {
			slog.Warn("live: operations listener stopped, reconnecting", "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
		}
	}
}
