// Package kube is Forge Live's Kubernetes provider. It runs in forge-agent,
// inside the cluster, under a read-only ServiceAccount.
//
// One client-go reflector per resource type (one watch per type for the
// whole cluster, never per application) feeds the provider directly: each
// object is trimmed, its health and summary computed, and it is then
// forgotten — the agent keeps a compact projection in the live.Tracker
// instead of an informer cache of full objects. Resources are grouped into
// applications by labels on the top-level objects; children (ReplicaSets,
// Pods) follow their controller owner.
package kube

import (
	"context"
	"fmt"
	"strings"
	"sync"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/priytamjeepandey/forge-ci/internal/live"
)

// Config says how objects map to applications.
type Config struct {
	// AppLabels name a top-level object's application: the first label
	// present wins. Default: app.kubernetes.io/instance, app.kubernetes.io/name.
	AppLabels []string
	// GroupLabel names the system an application is part of (shown and
	// filtered on). Default: app.kubernetes.io/part-of.
	GroupLabel string
	// RootSelector, when set, limits applications to top-level objects it
	// matches (a label selector, e.g. "app.kubernetes.io/part-of").
	RootSelector string
	// ExcludeNamespaces are never watched (filtered by the API server).
	ExcludeNamespaces []string
}

// kind is one watched resource type.
type kind struct {
	name, group, version string
	example              runtime.Object
	lw                   func(c kubernetes.Interface, fieldSelector string) cache.ListerWatcher
	owner                bool // controls other watched kinds (ReplicaSet → Pod)
}

type listWatcher[L runtime.Object] interface {
	List(context.Context, metav1.ListOptions) (L, error)
	Watch(context.Context, metav1.ListOptions) (watch.Interface, error)
}

// typed adapts a typed client's List/Watch, so the provider runs the same
// against a real API server and client-go's fake clientset.
func typed[L runtime.Object](get func(kubernetes.Interface) listWatcher[L]) func(kubernetes.Interface, string) cache.ListerWatcher {
	return func(c kubernetes.Interface, fieldSelector string) cache.ListerWatcher {
		x := get(c)
		return &cache.ListWatch{
			ListWithContextFunc: func(ctx context.Context, o metav1.ListOptions) (runtime.Object, error) {
				o.FieldSelector = fieldSelector
				return x.List(ctx, o)
			},
			WatchFuncWithContext: func(ctx context.Context, o metav1.ListOptions) (watch.Interface, error) {
				o.FieldSelector = fieldSelector
				return x.Watch(ctx, o)
			},
		}
	}
}

var kinds = []*kind{
	{name: "Deployment", group: "apps", version: "v1", example: &appsv1.Deployment{}, owner: true,
		lw: typed(func(c kubernetes.Interface) listWatcher[*appsv1.DeploymentList] { return c.AppsV1().Deployments("") })},
	{name: "ReplicaSet", group: "apps", version: "v1", example: &appsv1.ReplicaSet{}, owner: true,
		lw: typed(func(c kubernetes.Interface) listWatcher[*appsv1.ReplicaSetList] { return c.AppsV1().ReplicaSets("") })},
	{name: "StatefulSet", group: "apps", version: "v1", example: &appsv1.StatefulSet{}, owner: true,
		lw: typed(func(c kubernetes.Interface) listWatcher[*appsv1.StatefulSetList] { return c.AppsV1().StatefulSets("") })},
	{name: "DaemonSet", group: "apps", version: "v1", example: &appsv1.DaemonSet{}, owner: true,
		lw: typed(func(c kubernetes.Interface) listWatcher[*appsv1.DaemonSetList] { return c.AppsV1().DaemonSets("") })},
	{name: "Job", group: "batch", version: "v1", example: &batchv1.Job{}, owner: true,
		lw: typed(func(c kubernetes.Interface) listWatcher[*batchv1.JobList] { return c.BatchV1().Jobs("") })},
	{name: "Pod", version: "v1", example: &corev1.Pod{},
		lw: typed(func(c kubernetes.Interface) listWatcher[*corev1.PodList] { return c.CoreV1().Pods("") })},
	{name: "Service", version: "v1", example: &corev1.Service{},
		lw: typed(func(c kubernetes.Interface) listWatcher[*corev1.ServiceList] { return c.CoreV1().Services("") })},
	{name: "ConfigMap", version: "v1", example: &corev1.ConfigMap{},
		lw: typed(func(c kubernetes.Interface) listWatcher[*corev1.ConfigMapList] { return c.CoreV1().ConfigMaps("") })},
	{name: "PersistentVolumeClaim", version: "v1", example: &corev1.PersistentVolumeClaim{},
		lw: typed(func(c kubernetes.Interface) listWatcher[*corev1.PersistentVolumeClaimList] {
			return c.CoreV1().PersistentVolumeClaims("")
		})},
	{name: "Ingress", group: "networking.k8s.io", version: "v1", example: &networkingv1.Ingress{},
		lw: typed(func(c kubernetes.Interface) listWatcher[*networkingv1.IngressList] {
			return c.NetworkingV1().Ingresses("")
		})},
	{name: "HorizontalPodAutoscaler", group: "autoscaling", version: "v2", example: &autoscalingv2.HorizontalPodAutoscaler{},
		lw: typed(func(c kubernetes.Interface) listWatcher[*autoscalingv2.HorizontalPodAutoscalerList] {
			return c.AutoscalingV2().HorizontalPodAutoscalers("")
		})},
}

// eventKind is watched once every other kind is listed, so the first events
// already find their resources.
var eventKind = &kind{name: "Event", version: "v1", example: &corev1.Event{},
	lw: typed(func(c kubernetes.Interface) listWatcher[*corev1.EventList] { return c.CoreV1().Events("") })}

// AgentMemoryLimit is the Go soft memory limit forge-agent runs with
// (GOMEMLIMIT overrides it): below the 150 MB budget, so garbage is
// collected before the heap reaches it.
const AgentMemoryLimit = 120 << 20

// Provider watches one cluster.
type Provider struct {
	client   kubernetes.Interface
	cfg      Config
	selector labels.Selector // nil: every labelled top-level object
	// listWatch builds a kind's lister-watcher (the load test swaps in
	// synthetic ones).
	listWatch func(k *kind) cache.ListerWatcher
}

// New returns a provider for the cluster client talks to.
func New(client kubernetes.Interface, cfg Config) (*Provider, error) {
	if len(cfg.AppLabels) == 0 {
		cfg.AppLabels = []string{"app.kubernetes.io/instance", "app.kubernetes.io/name"}
	}
	if cfg.GroupLabel == "" {
		cfg.GroupLabel = "app.kubernetes.io/part-of"
	}
	p := &Provider{client: client, cfg: cfg}
	if cfg.RootSelector != "" {
		sel, err := labels.Parse(cfg.RootSelector)
		if err != nil {
			return nil, fmt.Errorf("root selector: %w", err)
		}
		p.selector = sel
	}
	fs := make([]string, 0, len(cfg.ExcludeNamespaces))
	for _, ns := range cfg.ExcludeNamespaces {
		if ns = strings.TrimSpace(ns); ns != "" {
			fs = append(fs, "metadata.namespace!="+ns)
		}
	}
	fieldSelector := strings.Join(fs, ",")
	p.listWatch = func(k *kind) cache.ListerWatcher { return k.lw(p.client, fieldSelector) }
	return p, nil
}

func (p *Provider) Name() string { return "kubernetes" }

func (p *Provider) Capabilities() live.Capabilities {
	return live.Capabilities{Events: true, Manifests: true, Watch: true}
}

// Watch runs one reflector per kind until ctx ends. It returns
// live.ErrRestart when a top-level object moved to another application (its
// whole subtree must follow, which a fresh report does simply).
func (p *Provider) Watch(ctx context.Context, sink live.Sink) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	w := &watcher{
		p: p, sink: sink, nodes: map[string]*node{}, apps: map[string]*live.App{},
		strs: map[string]string{}, healths: map[live.Health]*live.Health{},
		parked: map[string]map[string]parkedChild{}, parkedBy: map[string]string{},
		listedKinds: map[*kind]bool{}, listed: make(chan struct{}), restart: make(chan struct{}),
	}
	var wg sync.WaitGroup
	run := func(k *kind) {
		st := &store{w: w, k: k}
		lw := &pagedLister{lw: cache.ToListerWatcherWithContext(p.listWatch(k)), s: st}
		r := cache.NewReflectorWithOptions(lw, k.example, st, cache.ReflectorOptions{Name: "forge-live/" + k.name})
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.RunWithContext(ctx)
		}()
	}
	for _, k := range kinds {
		run(k)
	}
	defer wg.Wait()
	select {
	case <-ctx.Done():
		return nil
	case <-w.listed:
	}
	run(eventKind)
	sink.Synced()
	select {
	case <-ctx.Done():
		return nil
	case <-w.restart:
		return live.ErrRestart
	}
}

// pageSize is how many objects one list request returns.
const pageSize = 500

// pagedLister lists a kind a page at a time, handing each page to the
// watcher as it arrives, and gives the reflector an empty list carrying the
// resourceVersion to watch from. A reflector's own list (or watch-list)
// holds every object of the kind at once — at 100k pods, hundreds of MB —
// before its store sees any; this way peak memory is one page. The
// reflector still does what it is good at: watching from that version,
// bookmarks, backoff, and relisting when the version expires.
type pagedLister struct {
	lw cache.ListerWatcherWithContext
	s  *store
}

func (l *pagedLister) ListWithContext(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error) {
	// A list at resourceVersion "0" is served whole from the watch cache,
	// ignoring the limit; "" pages.
	opts.ResourceVersion, opts.ResourceVersionMatch, opts.Limit = "", "", pageSize
	seen := map[string]bool{}
	for {
		page, err := l.lw.ListWithContext(ctx, opts)
		if err != nil {
			return nil, err
		}
		items, err := meta.ExtractList(page)
		if err != nil {
			return nil, err
		}
		for _, obj := range items {
			if m, ok := obj.(metav1.Object); ok {
				seen[string(m.GetUID())] = true
			}
			l.s.w.apply(l.s.k, obj)
		}
		lm, err := meta.ListAccessor(page)
		if err != nil {
			return nil, err
		}
		if lm.GetContinue() == "" {
			l.s.listed = seen
			return &metav1.List{ListMeta: metav1.ListMeta{ResourceVersion: lm.GetResourceVersion()}}, nil
		}
		opts.Continue = lm.GetContinue()
	}
}

func (l *pagedLister) WatchWithContext(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error) {
	return l.lw.WatchWithContext(ctx, opts)
}

// List and Watch satisfy cache.ListerWatcher; the reflector calls the
// context versions.
func (l *pagedLister) List(opts metav1.ListOptions) (runtime.Object, error) {
	return l.ListWithContext(context.Background(), opts)
}

func (l *pagedLister) Watch(opts metav1.ListOptions) (watch.Interface, error) {
	return l.WatchWithContext(context.Background(), opts)
}

// IsWatchListSemanticsUnSupported keeps the reflector on the list path, which
// is the one that pages.
func (l *pagedLister) IsWatchListSemanticsUnSupported() bool { return true }

// store is the reflector's store for one kind: it hands every object to the
// watcher and keeps nothing.
type store struct {
	w      *watcher
	k      *kind
	listed map[string]bool // uids in the last list (set by pagedLister)
}

func (s *store) Add(obj any) error    { s.w.apply(s.k, obj); return nil }
func (s *store) Update(obj any) error { s.w.apply(s.k, obj); return nil }
func (s *store) Delete(obj any) error {
	if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = d.Obj
	}
	if m, ok := obj.(metav1.Object); ok && s.k != eventKind {
		s.w.remove(string(m.GetUID()))
	}
	return nil
}
func (s *store) Replace(_ []any, _ string) error { s.w.relisted(s.k, s.listed); return nil }
func (s *store) Resync() error                   { return nil }

// node is what the watcher remembers about each object: its application and
// its owner. ~100 bytes per object, whatever the object's size.
type node struct {
	kind     *kind
	app      *live.App // nil: not part of an application
	owner    string
	self     []string // []string{uid}: every child's Parents, shared
	children int32    // watched objects this one controls
}

type parkedChild struct {
	kind *kind
	res  live.Resource
}

// watcher groups the objects of every kind into applications.
type watcher struct {
	p    *Provider
	sink live.Sink

	mu      sync.Mutex
	nodes   map[string]*node
	apps    map[string]*live.App // interned by key
	strs    map[string]string
	healths map[live.Health]*live.Health
	// Objects whose owner has not been seen yet (watches of different kinds
	// are not ordered), by owner uid.
	parked   map[string]map[string]parkedChild
	parkedBy map[string]string

	listedKinds map[*kind]bool // kinds whose first list is in
	listed      chan struct{}
	restart     chan struct{}
	restarts    sync.Once
}

// ownerKinds are the watched kinds that control others, by apiVersion/kind.
var ownerKinds = func() map[string]bool {
	out := map[string]bool{}
	for _, k := range kinds {
		if k.owner {
			out[k.group+"/"+k.version+"/"+k.name] = true
		}
	}
	return out
}()

// ownerOf is the uid of obj's controller when that controller is a watched
// kind. Objects controlled by anything else (a CronJob, an operator's
// custom resource) are top-level.
func ownerOf(m metav1.Object) string {
	for _, ref := range m.GetOwnerReferences() {
		if ref.Controller != nil && *ref.Controller && ownerKinds[ref.APIVersion+"/"+ref.Kind] {
			return string(ref.UID)
		}
	}
	return ""
}

func (w *watcher) apply(k *kind, obj any) {
	if k == eventKind {
		if e, ok := obj.(*corev1.Event); ok {
			if ev, ok := event(e); ok {
				w.mu.Lock()
				if n := w.nodes[ev.ResourceUID]; n != nil && n.app != nil {
					w.sink.Event(*n.app, ev)
				}
				w.mu.Unlock()
			}
		}
		return
	}
	meta, res, ok := convert(k, obj)
	if !ok {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	owner := w.str(ownerOf(meta))
	w.intern(&res)
	var root *live.App
	if owner == "" {
		root = w.appFor(meta)
	}
	w.place(k, res, owner, root)
}

// Interning: thousands of pods share their namespace, images, node, owner
// and health; the tracker holds one copy of each instead of one per pod.
const maxInterned = 100_000

func (w *watcher) str(s string) string {
	if v, ok := w.strs[s]; ok {
		return v
	}
	if len(w.strs) >= maxInterned {
		clear(w.strs)
	}
	w.strs[s] = s
	return s
}

func (w *watcher) intern(r *live.Resource) {
	r.Namespace = w.str(r.Namespace)
	r.Summary.Ready, r.Summary.Status = w.str(r.Summary.Ready), w.str(r.Summary.Status)
	for i, img := range r.Summary.Images {
		r.Summary.Images[i] = w.str(img)
	}
	for i, kv := range r.Summary.Info {
		if kv.Name == "Node" {
			r.Summary.Info[i].Value = w.str(kv.Value)
		}
	}
	if r.Health != nil {
		if h, ok := w.healths[*r.Health]; ok {
			r.Health = h
		} else if len(w.healths) < maxInterned {
			w.healths[*r.Health] = r.Health
		}
	}
}

// appFor names a top-level object's application from its labels.
func (w *watcher) appFor(m metav1.Object) *live.App {
	l := m.GetLabels()
	name := ""
	for _, key := range w.p.cfg.AppLabels {
		if name = l[key]; name != "" {
			break
		}
	}
	if name == "" || (w.p.selector != nil && !w.p.selector.Matches(labels.Set(l))) {
		return nil
	}
	key := m.GetNamespace() + "/" + name
	a := w.apps[key]
	if a == nil {
		a = &live.App{Key: key, Name: name, Namespace: m.GetNamespace(), Group: l[w.p.cfg.GroupLabel]}
		w.apps[key] = a
	}
	return a
}

// place records res under its application — the root's, or its owner's —
// and settles any children that were waiting for it.
func (w *watcher) place(k *kind, res live.Resource, owner string, app *live.App) {
	uid := res.UID
	if owner != "" {
		on := w.nodes[owner]
		if on == nil {
			if w.parked[owner] == nil {
				w.parked[owner] = map[string]parkedChild{}
			}
			w.parked[owner][uid] = parkedChild{kind: k, res: res}
			w.parkedBy[uid] = owner
			return
		}
		app = on.app
		res.Parents = on.self
	}
	n := w.nodes[uid]
	if n == nil {
		n = &node{kind: k}
		if k.owner {
			n.self = []string{uid}
		}
		w.nodes[uid] = n
	} else if n.app != nil && (app == nil || n.app.Key != app.Key) {
		w.sink.Delete(*n.app, uid)
		if n.children > 0 {
			w.restarts.Do(func() { close(w.restart) })
		}
	}
	if n.owner != owner {
		if on := w.nodes[n.owner]; on != nil {
			on.children--
		}
		if on := w.nodes[owner]; on != nil {
			on.children++
		}
	}
	n.app, n.owner = app, owner
	if app != nil {
		w.sink.Upsert(*app, res)
	}
	if kids := w.parked[uid]; kids != nil {
		delete(w.parked, uid)
		for cuid, c := range kids {
			delete(w.parkedBy, cuid)
			w.place(c.kind, c.res, uid, nil)
		}
	}
}

func (w *watcher) remove(uid string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.removeLocked(uid)
}

func (w *watcher) removeLocked(uid string) {
	if owner, ok := w.parkedBy[uid]; ok {
		delete(w.parked[owner], uid)
		if len(w.parked[owner]) == 0 {
			delete(w.parked, owner)
		}
		delete(w.parkedBy, uid)
		return
	}
	n := w.nodes[uid]
	if n == nil {
		return
	}
	delete(w.nodes, uid)
	if on := w.nodes[n.owner]; on != nil {
		on.children--
	}
	if n.app != nil {
		w.sink.Delete(*n.app, uid)
	}
}

// relisted completes a (re)list of one kind (its objects were applied page
// by page): anything of that kind not in it is gone.
func (w *watcher) relisted(k *kind, seen map[string]bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if k != eventKind {
		for uid, n := range w.nodes {
			if n.kind == k && !seen[uid] {
				w.removeLocked(uid)
			}
		}
		for uid, owner := range w.parkedBy {
			if w.parked[owner][uid].kind == k && !seen[uid] {
				w.removeLocked(uid)
			}
		}
	}
	if k != eventKind && !w.listedKinds[k] {
		w.listedKinds[k] = true
		if len(w.listedKinds) == len(kinds) {
			close(w.listed)
		}
	}
}
