package kube

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/restmapper"

	"github.com/priytamjeepandey/forge-ci/internal/live"
)

// FieldManager owns the fields Forge applies (server-side apply).
const FieldManager = "forge"

// Applier changes the cluster on Forge's behalf. The agent has one only when
// it is given write access (deploy/live/agent-sync.yaml); without it the
// cluster stays read-only and Sync is not offered.
type Applier interface {
	// Apply server-side-applies obj and says what happened: created,
	// configured or unchanged (with dryRun: would-create, would-configure).
	Apply(ctx context.Context, obj *unstructured.Unstructured, dryRun, force bool) (action string, err error)
	// Delete removes an object (prune).
	Delete(ctx context.Context, group, version, kind, namespace, name string, dryRun bool) error
}

// NewApplier applies through the dynamic client, mapping kinds to resources
// with discovery (cached; re-read when a kind is unknown, e.g. a new CRD).
func NewApplier(dyn dynamic.Interface, disc discovery.DiscoveryInterface) Applier {
	return &dynApplier{dyn: dyn, mapper: restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(disc))}
}

type dynApplier struct {
	dyn    dynamic.Interface
	mapper *restmapper.DeferredDiscoveryRESTMapper
}

func (a *dynApplier) resource(gvk schema.GroupVersionKind, namespace string) (dynamic.ResourceInterface, error) {
	m, err := a.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if meta.IsNoMatchError(err) {
		a.mapper.Reset()
		m, err = a.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	}
	if err != nil {
		return nil, err
	}
	if m.Scope.Name() == meta.RESTScopeNameNamespace {
		return a.dyn.Resource(m.Resource).Namespace(namespace), nil
	}
	return a.dyn.Resource(m.Resource), nil
}

func (a *dynApplier) Apply(ctx context.Context, obj *unstructured.Unstructured, dryRun, force bool) (string, error) {
	ri, err := a.resource(obj.GroupVersionKind(), obj.GetNamespace())
	if err != nil {
		return "", err
	}
	before, err := ri.Get(ctx, obj.GetName(), metav1.GetOptions{})
	exists := err == nil
	if err != nil && !apierrors.IsNotFound(err) {
		return "", err
	}
	opts := metav1.ApplyOptions{FieldManager: FieldManager, Force: force}
	if dryRun {
		opts.DryRun = []string{metav1.DryRunAll}
	}
	after, err := ri.Apply(ctx, obj.GetName(), obj, opts)
	if err != nil {
		return "", err
	}
	switch {
	case !exists && dryRun:
		return "would-create", nil
	case !exists:
		return "created", nil
	case after.GetResourceVersion() == before.GetResourceVersion():
		return "unchanged", nil
	case dryRun:
		return "would-configure", nil
	}
	return "configured", nil
}

func (a *dynApplier) Delete(ctx context.Context, group, version, kind, namespace, name string, dryRun bool) error {
	ri, err := a.resource(schema.GroupVersionKind{Group: group, Version: version, Kind: kind}, namespace)
	if err != nil {
		return err
	}
	opts := metav1.DeleteOptions{PropagationPolicy: ptr(metav1.DeletePropagationForeground)}
	if dryRun {
		opts.DryRun = []string{metav1.DryRunAll}
	}
	err = ri.Delete(ctx, name, opts)
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func ptr[T any](v T) *T { return &v }

// applyOrder: what others depend on first (namespaces, config, storage),
// workloads next, what routes to them last.
var applyOrder = map[string]int{
	"Namespace": 0, "CustomResourceDefinition": 0, "ServiceAccount": 1, "Role": 1, "ClusterRole": 1, "RoleBinding": 2,
	"ClusterRoleBinding": 2, "ConfigMap": 3, "Secret": 3, "PersistentVolumeClaim": 3, "Service": 4,
	"Deployment": 5, "StatefulSet": 5, "DaemonSet": 5, "Job": 5, "CronJob": 5, "HorizontalPodAutoscaler": 6, "Ingress": 7,
}

// Operate runs one operation from Forge.
func (p *Provider) Operate(ctx context.Context, op live.Op, out func([]byte)) live.OpResult {
	p.mu.Lock()
	w := p.current
	p.mu.Unlock()
	if w == nil {
		return live.OpResult{Message: "the agent is still reading the cluster; try again in a moment"}
	}
	switch op.Kind {
	case live.OpRefresh:
		return w.refreshNow(ctx, op.Hard)
	case live.OpSync, live.OpRollback:
		return w.syncApp(ctx, op)
	case live.OpLogs:
		return p.logs(ctx, op, out)
	}
	return live.OpResult{Message: "unknown operation " + op.Kind}
}

func (w *watcher) refreshNow(ctx context.Context, hard bool) live.OpResult {
	if w.p.repo == nil {
		return live.OpResult{OK: true, Message: "Watching live; nothing to refresh"}
	}
	r := refreshReq{hard: hard, done: make(chan struct{})}
	select {
	case w.refresh <- r:
	case <-ctx.Done():
		return live.OpResult{Message: ctx.Err().Error()}
	}
	select {
	case <-r.done:
	case <-ctx.Done():
		return live.OpResult{Message: ctx.Err().Error()}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return live.OpResult{OK: true, Message: "Refreshed from " + shortSHA(w.head), Revision: w.head}
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

// syncApp applies the app's desired state at op.Revision (the branch tip
// when empty): in dependency order, server-side, as field manager "forge";
// with Prune, top-level objects of the app that are no longer desired go.
func (w *watcher) syncApp(ctx context.Context, op live.Op) live.OpResult {
	repo, applier := w.p.repo, w.p.applier
	if repo == nil || applier == nil {
		return live.OpResult{Message: "this agent has no write access (deploy/live/agent-sync.yaml)"}
	}
	w.mu.Lock()
	rev := op.Revision
	if rev == "" {
		rev = w.head
	}
	a := w.apps[op.App.Key]
	w.mu.Unlock()
	if a == nil {
		return live.OpResult{Message: "the agent does not know this application"}
	}
	if _, err := repo.fetch(ctx); err != nil {
		return live.OpResult{Message: err.Error()}
	}
	r, err := repo.render(ctx, rev, repo.appPath(*a), a.Namespace, a.Name)
	if err != nil {
		return live.OpResult{Message: "render: " + err.Error(), Revision: rev}
	}
	only := map[string]bool{}
	for _, k := range op.Resources {
		only[k] = true
	}
	objs := r.objects
	sort.SliceStable(objs, func(i, j int) bool { return order(objs[i].GetKind()) < order(objs[j].GetKind()) })
	res := live.OpResult{OK: true, Revision: rev}
	desired := map[string]bool{}
	for _, o := range objs {
		key := keyOf(o)
		desired[key] = true
		if len(only) > 0 && !only[key] {
			continue
		}
		rr := live.ResourceResult{Key: key, Kind: o.GetKind(), Name: o.GetName()}
		action, err := applier.Apply(ctx, o, op.DryRun, op.Force)
		if err != nil {
			rr.Action, rr.Message, res.OK = "failed", err.Error(), false
		} else {
			rr.Action = action
		}
		res.Resources = append(res.Resources, rr)
	}
	if op.Prune {
		w.mu.Lock()
		var keys []string
		for key := range w.rootsByApp[a.Key] {
			if !desired[key] && (len(only) == 0 || only[key]) {
				keys = append(keys, key)
			}
		}
		w.mu.Unlock()
		sort.Strings(keys)
		for i := range keys {
			f := strings.SplitN(keys[i], "/", 4)
			version := versionOf(f[0], f[1])
			rr := live.ResourceResult{Key: keys[i], Kind: f[1], Name: f[3], Action: "pruned"}
			if op.DryRun {
				rr.Action = "would-prune"
			}
			if err := applier.Delete(ctx, f[0], version, f[1], f[2], f[3], op.DryRun); err != nil {
				rr.Action, rr.Message, res.OK = "failed", err.Error(), false
			}
			res.Resources = append(res.Resources, rr)
		}
	}
	failed := 0
	for _, rr := range res.Resources {
		if rr.Action == "failed" {
			failed++
		}
	}
	verb := "Synced"
	if op.DryRun {
		verb = "Dry run of"
	}
	res.Message = fmt.Sprintf("%s %d resources to %s", verb, len(res.Resources)-failed, shortSHA(rev))
	if failed > 0 {
		res.Message = fmt.Sprintf("%d of %d resources failed", failed, len(res.Resources))
	}
	return res
}

func order(kind string) int {
	if n, ok := applyOrder[kind]; ok {
		return n
	}
	return 8
}

// versionOf is a watched kind's API version.
func versionOf(group, kind string) string {
	for _, k := range kinds {
		if k.group == group && k.name == kind {
			return k.version
		}
	}
	return "v1"
}

// Log streams are bounded: a page follows a pod for a while, not forever.
const (
	maxLogBytes    = 8 << 20
	maxLogDuration = 15 * time.Minute
)

func (p *Provider) logs(ctx context.Context, op live.Op, out func([]byte)) live.OpResult {
	ns, name, ok := strings.Cut(op.Pod, "/")
	if !ok {
		ns, name = op.App.Namespace, op.Pod
	}
	opts := &corev1.PodLogOptions{Container: op.Container, Follow: op.Follow}
	tail := int64(op.Tail)
	if tail <= 0 {
		tail = 500
	}
	opts.TailLines = &tail
	ctx, cancel := context.WithTimeout(ctx, maxLogDuration)
	defer cancel()
	stream, err := p.client.CoreV1().Pods(ns).GetLogs(name, opts).Stream(ctx)
	if err != nil {
		return live.OpResult{Message: err.Error()}
	}
	defer stream.Close()
	buf := make([]byte, 32<<10)
	total := 0
	for total < maxLogBytes {
		n, err := stream.Read(buf)
		if n > 0 {
			total += n
			out(append([]byte(nil), buf[:n]...))
		}
		if errors.Is(err, io.EOF) || ctx.Err() != nil {
			break
		}
		if err != nil {
			return live.OpResult{Message: err.Error()}
		}
	}
	return live.OpResult{OK: true, Message: fmt.Sprintf("%d bytes", total)}
}
