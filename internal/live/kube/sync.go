package kube

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
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
	// Get reads an object (nil when it does not exist): a sync waits on
	// what it applied.
	Get(ctx context.Context, group, version, kind, namespace, name string) (*unstructured.Unstructured, error)
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

func (a *dynApplier) Get(ctx context.Context, group, version, kind, namespace, name string) (*unstructured.Unstructured, error) {
	ri, err := a.resource(schema.GroupVersionKind{Group: group, Version: version, Kind: kind}, namespace)
	if err != nil {
		return nil, err
	}
	u, err := ri.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	return u, err
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
		return w.syncApp(ctx, op, out)
	case live.OpLogs:
		return p.logs(ctx, op, out)
	case live.OpPromote, live.OpAbort:
		return w.control(ctx, op)
	}
	return live.OpResult{Message: "unknown operation " + op.Kind}
}

// control hands a promote or abort to the app's running rollout. With none
// running here (the agent restarted mid-rollout), abort still removes the
// app's canaries; promote says to sync.
func (w *watcher) control(ctx context.Context, op live.Op) live.OpResult {
	if w.p.signal(op) {
		verb := "Promote"
		switch {
		case op.Kind == live.OpAbort:
			verb = "Abort"
		case !op.Full:
			verb = "Resume"
		}
		return live.OpResult{OK: true, Message: verb + " sent to the running rollout"}
	}
	if op.Kind == live.OpPromote {
		return live.OpResult{Message: "no rollout is running for this application; sync to start one"}
	}
	w.mu.Lock()
	var canaries []string
	for key := range w.rootsByApp[op.App.Key] {
		if rt := w.roots[key]; rt != nil && rt.canary {
			canaries = append(canaries, key)
		}
	}
	w.mu.Unlock()
	if len(canaries) == 0 || w.p.applier == nil {
		return live.OpResult{OK: true, Message: "no rollout is running for this application"}
	}
	res := live.OpResult{OK: true, Aborted: true}
	for _, key := range canaries {
		f := strings.SplitN(key, "/", 4)
		rr := live.ResourceResult{Key: key, Kind: f[1], Name: f[3], Action: "pruned"}
		if err := w.p.applier.Delete(ctx, f[0], versionOf(f[0], f[1]), f[1], f[2], f[3], false); err != nil {
			rr.Action, rr.Message, res.OK = "failed", err.Error(), false
		}
		res.Resources = append(res.Resources, rr)
	}
	res.Message = fmt.Sprintf("Removed %d canar%s left by an interrupted rollout", len(canaries), map[bool]string{true: "y", false: "ies"}[len(canaries) == 1])
	return res
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
// when empty), server-side as field manager "forge": phase by phase and wave
// by wave (waves.go), in dependency order within a wave. With Prune, what
// Forge applied that Git no longer has goes, after the last Sync wave.
func (w *watcher) syncApp(ctx context.Context, op live.Op, out func([]byte)) live.OpResult {
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
	w.mu.Lock()
	path, decl := w.pathOf(*a), w.declared[a.Key]
	w.mu.Unlock()
	r, err := repo.render(ctx, rev, path, a.Namespace, a.Name)
	if err != nil {
		return live.OpResult{Message: "render: " + err.Error(), Revision: rev}
	}
	s := &syncRun{w: w, op: op, app: a, applier: applier, out: out, pods: w.p.listPods}
	var ns *live.ResourceResult
	if decl.CreateNamespace && !op.DryRun && a.Namespace != "" {
		if rr, did := s.ensureNamespace(ctx, a.Namespace); did {
			ns = &rr
		}
	}
	digest := digestOf(r.objects) // before the sync names generated hooks
	res := s.execute(ctx, r.objects, rev)
	res.Digest = digest
	if ns != nil {
		res.Resources = append([]live.ResourceResult{*ns}, res.Resources...)
		if ns.Action == "failed" {
			res.OK = false
		}
	}
	return res
}

func (p *Provider) listPods(ctx context.Context, namespace, selector string) ([]corev1.Pod, error) {
	l, err := p.client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, err
	}
	return l.Items, nil
}

// execute runs a sync of objs (the app rendered at rev).
func (s *syncRun) execute(ctx context.Context, objs []*unstructured.Unstructured, rev string) live.OpResult {
	op := s.op
	s.only, s.res, s.rev = map[string]bool{}, live.OpResult{OK: true, Revision: rev}, rev
	for _, k := range op.Resources {
		s.only[k] = true
	}
	objs, specs, err := rollouts(objs)
	if err != nil {
		return live.OpResult{Message: err.Error(), Revision: rev}
	}
	// Rollbacks go straight back: an emergency is not the time to go slowly.
	if op.Kind != live.OpRollback && !op.DryRun {
		s.rollouts = specs
	}
	desired := map[string]bool{}
	for _, o := range objs {
		desired[keyOf(o)] = true
	}
	steps, failHooks := plan(objs)
	pruned := !op.Prune
	for i, st := range steps {
		if st.phase == postSync && !pruned {
			pruned = true
			if !s.prune(ctx, desired) {
				break
			}
		}
		if !s.run(ctx, st, i < len(steps)-1) {
			break
		}
	}
	if s.res.OK && !pruned {
		s.prune(ctx, desired)
	}
	if !s.res.OK && len(failHooks) > 0 && len(s.only) == 0 && !op.DryRun {
		s.run(ctx, &step{phase: syncFail, objs: failHooks, hooks: hooksOf(failHooks)}, false)
		s.res.OK = false
	}
	failed, hooks, removed := 0, 0, 0
	for _, rr := range s.res.Resources {
		switch {
		case rr.Action == "failed":
			failed++
		case rr.Phase != "":
			hooks++
		case strings.HasSuffix(rr.Action, "prune"), rr.Action == "pruned":
			removed++
		}
	}
	verb := "Synced"
	if op.DryRun {
		verb = "Dry run of"
	}
	n := len(s.res.Resources) - hooks - removed - failed
	s.res.Message = fmt.Sprintf("%s %d resources to %s", verb, n, shortSHA(rev))
	if removed > 0 {
		s.res.Message += fmt.Sprintf(", pruned %d", removed)
	}
	if hooks > 0 {
		s.res.Message += fmt.Sprintf(", %d hook%s", hooks, plural(hooks))
	}
	switch {
	case s.fail != "":
		s.res.Message = s.fail
	case failed > 0:
		s.res.Message = fmt.Sprintf("%d of %d resources failed", failed, len(s.res.Resources))
	}
	return s.res
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func hooksOf(objs []*unstructured.Unstructured) map[*unstructured.Unstructured]hook {
	out := map[*unstructured.Unstructured]hook{}
	for _, o := range objs {
		out[o] = hookOf(o.GetAnnotations())
	}
	return out
}

// Waiting: a wave has this long to become healthy (its hooks to complete),
// checked this often.
var (
	waveTimeout = 5 * time.Minute
	wavePoll    = 2 * time.Second
)

// syncRun is one sync in progress.
type syncRun struct {
	w       *watcher
	op      live.Op
	app     *live.App
	applier Applier
	out     func([]byte)
	only    map[string]bool // selected resource keys; none = all (a selective sync runs no hooks)
	res     live.OpResult
	fail    string // why the sync stopped, when a wave did
	// Rollouts by Deployment name (from the rendered app); pods and client
	// serve their analysis.
	rollouts map[string]*RolloutSpec
	rev      string
	pods     podLister
	client   *http.Client
}

func (s *syncRun) httpClient() *http.Client {
	if s.client != nil {
		return s.client
	}
	return &http.Client{Timeout: 2 * time.Second}
}

func (s *syncRun) say(format string, a ...any) {
	if s.out != nil {
		s.out([]byte(fmt.Sprintf(format, a...) + "\n"))
	}
}

// run applies one step and, when wait (or the step has hooks), waits for it
// to be healthy. It reports whether the sync may go on.
func (s *syncRun) run(ctx context.Context, st *step, wait bool) bool {
	var applied []*unstructured.Unstructured
	idx := map[*unstructured.Unstructured]int{}
	for _, o := range st.objs {
		h, isHook := st.hooks[o]
		if isHook && len(s.only) > 0 || !isHook && len(s.only) > 0 && !s.only[keyOf(o)] {
			continue
		}
		if isHook && o.GetName() == "" {
			o.SetName(o.GetGenerateName() + strconv.FormatInt(s.op.ID, 10))
		}
		rr := live.ResourceResult{Key: keyOf(o), Kind: o.GetKind(), Name: o.GetName(), Wave: st.wave}
		if isHook {
			rr.Phase = st.phase
		}
		switch {
		case isHook && s.op.DryRun:
			rr.Action = "would-run"
		case isHook && h.delete[BeforeHookCreation] && !s.gone(ctx, o):
			rr.Action, rr.Message = "failed", "the previous run of this hook is still being deleted"
		case !isHook && o.GetKind() == "Deployment" && s.rollouts[o.GetName()] != nil:
			// Progressive delivery: this blocks until promoted or aborted.
			res, ok := s.rollout(ctx, o, s.rollouts[o.GetName()], s.rev)
			res.Wave = rr.Wave
			rr = res
			if ok {
				applied = append(applied, o)
			}
		default:
			action, err := s.applier.Apply(ctx, o, s.op.DryRun, s.op.Force)
			if err != nil {
				rr.Action, rr.Message = "failed", err.Error()
			} else {
				rr.Action = action
				applied = append(applied, o)
			}
		}
		if rr.Action == "failed" || rr.Action == "aborted" {
			s.res.OK = false
		}
		idx[o] = len(s.res.Resources)
		s.res.Resources = append(s.res.Resources, rr)
	}
	if !s.res.OK || s.op.DryRun || len(applied) == 0 || !wait && len(st.hooks) == 0 {
		return s.res.OK
	}
	return s.wait(ctx, st, applied, idx)
}

// wait polls the step's objects until every resource is healthy and every
// hook has completed, one fails, or the wave runs out of time.
func (s *syncRun) wait(ctx context.Context, st *step, objs []*unstructured.Unstructured, idx map[*unstructured.Unstructured]int) bool {
	label := fmt.Sprintf("%s wave %d", st.phase, st.wave)
	s.say("%s: waiting for %d resource%s", label, len(objs), plural(len(objs)))
	deadline := time.Now().Add(waveTimeout)
	pending := objs
	for {
		var still []*unstructured.Unstructured
		var why string
		for _, o := range pending {
			_, isHook := st.hooks[o]
			exists, hl, err := s.health(ctx, o)
			switch {
			case err != nil || !exists:
				still = append(still, o) // not there yet, or the API hiccuped
				why = fmt.Sprintf("%s %s to appear", o.GetKind(), o.GetName())
				if err != nil {
					why = err.Error()
				}
			case hl == nil, hl.Status == live.Healthy, hl.Status == live.Suspended && !isHook:
				if isHook {
					s.res.Resources[idx[o]].Action = "succeeded"
					s.say("%s: hook %s %s succeeded", label, o.GetKind(), o.GetName())
				}
			case hl.Status == live.Degraded:
				rr := &s.res.Resources[idx[o]]
				rr.Action, rr.Message = "failed", hl.Message
				s.res.OK = false
				s.fail = fmt.Sprintf("%s: %s %s is Degraded: %s", label, o.GetKind(), o.GetName(), hl.Message)
			default:
				still = append(still, o)
				why = fmt.Sprintf("%s %s: %s", o.GetKind(), o.GetName(), hl.Status)
				if hl.Message != "" {
					why += ": " + hl.Message
				}
			}
		}
		if !s.res.OK || len(still) == 0 {
			s.deleteHooks(ctx, st)
			if s.res.OK {
				s.say("%s: healthy", label)
			} else {
				s.say("%s", s.fail)
			}
			return s.res.OK
		}
		if time.Now().After(deadline) {
			s.res.OK = false
			s.fail = fmt.Sprintf("%s: timed out after %s waiting for %s", label, waveTimeout, why)
			for _, o := range still {
				if rr := &s.res.Resources[idx[o]]; rr.Phase != "" {
					rr.Action, rr.Message = "failed", "timed out"
				}
			}
			s.say("%s", s.fail)
			s.deleteHooks(ctx, st)
			return false
		}
		pending = still
		select {
		case <-ctx.Done():
			s.res.OK, s.fail = false, ctx.Err().Error()
			return false
		case <-time.After(wavePoll):
		}
	}
}

// health reads a live object's health (nil for kinds that have none).
func (s *syncRun) health(ctx context.Context, o *unstructured.Unstructured) (exists bool, h *live.Health, err error) {
	group, version := splitAPIVersion(o.GetAPIVersion())
	u, err := s.applier.Get(ctx, group, version, o.GetKind(), o.GetNamespace(), o.GetName())
	if err != nil || u == nil {
		return false, nil, err
	}
	return true, healthOf(u), nil
}

// deleteHooks removes the step's hooks whose delete policy says so.
func (s *syncRun) deleteHooks(ctx context.Context, st *step) {
	for o, h := range st.hooks {
		i := slices.IndexFunc(s.res.Resources, func(rr live.ResourceResult) bool { return rr.Key == keyOf(o) })
		if i < 0 {
			continue
		}
		action := s.res.Resources[i].Action
		if action == "succeeded" && h.delete[HookSucceeded] || action == "failed" && h.delete[HookFailed] {
			group, version := splitAPIVersion(o.GetAPIVersion())
			if err := s.applier.Delete(ctx, group, version, o.GetKind(), o.GetNamespace(), o.GetName(), false); err != nil {
				s.say("could not delete hook %s %s: %v", o.GetKind(), o.GetName(), err)
			}
		}
	}
}

// gone deletes a hook's previous run and waits until it is gone.
func (s *syncRun) gone(ctx context.Context, o *unstructured.Unstructured) bool {
	group, version := splitAPIVersion(o.GetAPIVersion())
	get := func() bool {
		u, err := s.applier.Get(ctx, group, version, o.GetKind(), o.GetNamespace(), o.GetName())
		return err == nil && u == nil
	}
	if get() {
		return true
	}
	if err := s.applier.Delete(ctx, group, version, o.GetKind(), o.GetNamespace(), o.GetName(), false); err != nil {
		return false
	}
	for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); {
		if get() {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(wavePoll / 2):
		}
	}
	return false
}

// prune deletes what Forge applied to the app that Git no longer has, the
// dependents first (reverse apply order). Nothing someone else created is
// ever pruned, nor a hook.
func (s *syncRun) prune(ctx context.Context, desired map[string]bool) bool {
	w := s.w
	w.mu.Lock()
	var keys []string
	for key := range w.rootsByApp[s.app.Key] {
		rt := w.roots[key]
		if rt != nil && rt.forge && !rt.hook && !rt.canary && !desired[key] && (len(s.only) == 0 || s.only[key]) {
			keys = append(keys, key)
		}
	}
	w.mu.Unlock()
	kindOf := func(key string) string { return strings.SplitN(key, "/", 3)[1] }
	slices.SortFunc(keys, func(a, b string) int {
		return cmp.Or(cmp.Compare(order(kindOf(b)), order(kindOf(a))), cmp.Compare(a, b))
	})
	for _, key := range keys {
		f := strings.SplitN(key, "/", 4)
		rr := live.ResourceResult{Key: key, Kind: f[1], Name: f[3], Action: "pruned"}
		if s.op.DryRun {
			rr.Action = "would-prune"
		}
		if err := s.applier.Delete(ctx, f[0], versionOf(f[0], f[1]), f[1], f[2], f[3], s.op.DryRun); err != nil {
			rr.Action, rr.Message, s.res.OK = "failed", err.Error(), false
		}
		s.res.Resources = append(s.res.Resources, rr)
	}
	return s.res.OK
}

func splitAPIVersion(gv string) (group, version string) {
	if i := strings.Index(gv, "/"); i >= 0 {
		return gv[:i], gv[i+1:]
	}
	return "", gv
}

// healthOf computes a fetched object's health the way the watch does (nil
// for kinds Forge does not watch, which have none).
func healthOf(u *unstructured.Unstructured) *live.Health {
	group, _ := splitAPIVersion(u.GetAPIVersion())
	for _, k := range kinds {
		if k.group != group || k.name != u.GetKind() {
			continue
		}
		obj := k.example.DeepCopyObject()
		if runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, obj) != nil {
			return nil
		}
		_, r, ok := convert(k, obj)
		if !ok {
			return nil
		}
		return r.Health
	}
	return nil
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
