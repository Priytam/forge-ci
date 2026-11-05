package kube

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/priytamjeepandey/forge-ci/internal/live"
)

func TestHookOf(t *testing.T) {
	for _, tc := range []struct {
		ann    map[string]string
		phase  string
		wave   int
		delete string
	}{
		{nil, "", 0, ""},
		{map[string]string{annWave: "-2"}, "", -2, ""},
		{map[string]string{annHook: "PreSync"}, "PreSync", 0, BeforeHookCreation},
		{map[string]string{annHook: "PostSync", annWave: "3", annHookDelete: "HookSucceeded"}, "PostSync", 3, HookSucceeded},
		{map[string]string{annHook: "Skip"}, skipHook, 0, BeforeHookCreation},
		{map[string]string{"helm.sh/hook": "pre-install,pre-upgrade", "helm.sh/hook-weight": "5",
			"helm.sh/hook-delete-policy": "hook-succeeded"}, "PreSync", 5, HookSucceeded},
		{map[string]string{"helm.sh/hook": "test"}, skipHook, 0, BeforeHookCreation},
	} {
		h := hookOf(tc.ann)
		if h.phase != tc.phase || h.wave != tc.wave || (tc.delete != "" && !h.delete[tc.delete]) {
			t.Errorf("hookOf(%v) = %+v", tc.ann, h)
		}
	}
}

func objects(t *testing.T, docs string) []*unstructured.Unstructured {
	t.Helper()
	var out []*unstructured.Unstructured
	for _, d := range strings.Split(docs, "\n---\n") {
		var m map[string]any
		if err := yaml.Unmarshal([]byte(d), &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, &unstructured.Unstructured{Object: m})
	}
	return out
}

const waveApp = `apiVersion: batch/v1
kind: Job
metadata: {name: smoke, namespace: shop, annotations: {forge.dev/hook: PostSync, forge.dev/hook-delete-policy: HookSucceeded}}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: web, namespace: shop}
spec: {replicas: 2}
---
apiVersion: v1
kind: Service
metadata: {name: web, namespace: shop}
---
apiVersion: batch/v1
kind: Job
metadata: {name: migrate, namespace: shop, annotations: {forge.dev/hook: PreSync}}
---
apiVersion: v1
kind: ConfigMap
metadata: {name: schema, namespace: shop, annotations: {forge.dev/sync-wave: "-1"}}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: worker, namespace: shop, annotations: {forge.dev/sync-wave: "1"}}
spec: {replicas: 1}
---
apiVersion: batch/v1
kind: Job
metadata: {name: notify, namespace: shop, annotations: {forge.dev/hook: SyncFail}}
---
apiVersion: v1
kind: Pod
metadata: {name: test, namespace: shop, annotations: {helm.sh/hook: test}}`

func TestPlan(t *testing.T) {
	steps, fail := plan(objects(t, waveApp))
	var got []string
	for _, s := range steps {
		var names []string
		for _, o := range s.objs {
			names = append(names, o.GetKind()+"/"+o.GetName())
		}
		got = append(got, fmt.Sprintf("%s %d: %s", s.phase, s.wave, strings.Join(names, ",")))
	}
	want := []string{
		"PreSync 0: Job/migrate",
		"Sync -1: ConfigMap/schema",
		"Sync 0: Service/web,Deployment/web",
		"Sync 1: Deployment/worker",
		"PostSync 0: Job/smoke",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("plan:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if len(fail) != 1 || fail[0].GetName() != "notify" {
		t.Errorf("SyncFail hooks = %v", fail)
	}
}

// clusterApplier is a cluster in memory: what it applies exists, and reads
// back with a status — Deployments rolled out, Jobs complete — unless the
// test says otherwise.
type clusterApplier struct {
	mu      sync.Mutex
	objs    map[string]*unstructured.Unstructured
	log     []string
	failJob map[string]bool // Jobs that fail
	stuck   map[string]bool // Deployments that never roll out
}

func newClusterApplier() *clusterApplier {
	return &clusterApplier{objs: map[string]*unstructured.Unstructured{}, failJob: map[string]bool{}, stuck: map[string]bool{}}
}

func (c *clusterApplier) Apply(_ context.Context, o *unstructured.Unstructured, dryRun, _ bool) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if dryRun {
		return "would-configure", nil
	}
	c.log = append(c.log, "apply "+o.GetKind()+"/"+o.GetName())
	c.objs[o.GetKind()+"/"+o.GetName()] = o.DeepCopy()
	return "configured", nil
}

func (c *clusterApplier) Delete(_ context.Context, _, _, kind, _, name string, dryRun bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !dryRun {
		c.log = append(c.log, "delete "+kind+"/"+name)
		delete(c.objs, kind+"/"+name)
	}
	return nil
}

func (c *clusterApplier) Get(_ context.Context, _, _, kind, _, name string) (*unstructured.Unstructured, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	o := c.objs[kind+"/"+name]
	if o == nil {
		return nil, nil
	}
	o = o.DeepCopy()
	switch kind {
	case "Deployment":
		v, _, _ := unstructured.NestedFieldNoCopy(o.Object, "spec", "replicas")
		n, _ := v.(int64)
		if f, ok := v.(float64); ok { // decoded from YAML
			n = int64(f)
		}
		if c.stuck[name] {
			n = 0
		}
		_ = unstructured.SetNestedField(o.Object, map[string]any{"replicas": n, "updatedReplicas": n, "availableReplicas": n, "readyReplicas": n}, "status")
	case "Job":
		cond := "Complete"
		if c.failJob[name] {
			cond = "Failed"
		}
		_ = unstructured.SetNestedSlice(o.Object, []any{map[string]any{"type": cond, "status": "True", "message": "BackoffLimitExceeded"}}, "status", "conditions")
	}
	return o, nil
}

func newRun(c *clusterApplier, op live.Op) *syncRun {
	w := &watcher{roots: map[string]*root{}, rootsByApp: map[string]map[string]bool{}}
	return &syncRun{w: w, op: op, app: &live.App{Key: "shop/web"}, applier: c}
}

func TestSyncWavesAndHooks(t *testing.T) {
	defer func(p time.Duration) { wavePoll = p }(wavePoll)
	wavePoll = time.Millisecond
	c := newClusterApplier()
	c.objs["Job/migrate"] = &unstructured.Unstructured{Object: map[string]any{}} // last sync's run
	var out strings.Builder
	s := newRun(c, live.Op{Kind: live.OpSync})
	s.out = func(b []byte) { out.Write(b) }
	res := s.execute(context.Background(), objects(t, waveApp), "abc1234")
	if !res.OK {
		t.Fatalf("sync failed: %+v", res)
	}
	want := []string{
		"delete Job/migrate", "apply Job/migrate", // BeforeHookCreation, then the PreSync hook
		"apply ConfigMap/schema", "apply Service/web", "apply Deployment/web", "apply Deployment/worker",
		"apply Job/smoke", "delete Job/smoke", // PostSync; deleted once it succeeded
	}
	if strings.Join(c.log, "\n") != strings.Join(want, "\n") {
		t.Errorf("order:\n%s", strings.Join(c.log, "\n"))
	}
	if res.Message != "Synced 4 resources to abc1234, 2 hooks" {
		t.Errorf("message = %q", res.Message)
	}
	for _, rr := range res.Resources {
		if rr.Phase != "" && rr.Action != "succeeded" {
			t.Errorf("hook %s: %s", rr.Name, rr.Action)
		}
	}
	if !strings.Contains(out.String(), "Sync wave 0: healthy") {
		t.Errorf("progress:\n%s", out.String())
	}
}

func TestSyncHookFailureRunsSyncFail(t *testing.T) {
	defer func(p time.Duration) { wavePoll = p }(wavePoll)
	wavePoll = time.Millisecond
	c := newClusterApplier()
	c.failJob["migrate"] = true
	res := newRun(c, live.Op{Kind: live.OpSync}).execute(context.Background(), objects(t, waveApp), "abc1234")
	if res.OK || !strings.Contains(res.Message, "PreSync wave 0: Job migrate is Degraded: BackoffLimitExceeded") {
		t.Fatalf("result = %+v", res)
	}
	if strings.Join(c.log, ",") != "apply Job/migrate,apply Job/notify" {
		t.Errorf("after a failed PreSync hook: %v", c.log)
	}
}

func TestSyncWaveTimeout(t *testing.T) {
	defer func(p, d time.Duration) { wavePoll, waveTimeout = p, d }(wavePoll, waveTimeout)
	wavePoll, waveTimeout = time.Millisecond, 20*time.Millisecond
	c := newClusterApplier()
	c.stuck["web"] = true
	res := newRun(c, live.Op{Kind: live.OpSync}).execute(context.Background(), objects(t, waveApp), "abc1234")
	if res.OK || !strings.Contains(res.Message, "Sync wave 0: timed out") || !strings.Contains(res.Message, "Deployment web: Progressing") {
		t.Fatalf("result = %+v", res)
	}
	for _, l := range c.log {
		if strings.Contains(l, "worker") {
			t.Errorf("wave 1 ran after wave 0 timed out: %v", c.log)
		}
	}
}

// A dry run applies every step server-side without waiting or running hooks;
// a selective sync runs no hooks at all.
func TestSyncDryRunAndSelective(t *testing.T) {
	c := newClusterApplier()
	res := newRun(c, live.Op{Kind: live.OpSync, DryRun: true}).execute(context.Background(), objects(t, waveApp), "abc1234")
	if !res.OK || len(c.log) != 0 {
		t.Fatalf("dry run = %+v, log %v", res, c.log)
	}
	runs := 0
	for _, rr := range res.Resources {
		if rr.Action == "would-run" {
			runs++
		}
	}
	if runs != 2 {
		t.Errorf("dry run hooks = %d, want 2", runs)
	}
	res = newRun(c, live.Op{Kind: live.OpSync, Resources: []string{"apps/Deployment/shop/worker"}}).execute(context.Background(), objects(t, waveApp), "abc1234")
	if !res.OK || strings.Join(c.log, ",") != "apply Deployment/worker" {
		t.Errorf("selective sync = %+v, log %v", res, c.log)
	}
}

// Prune deletes only what Forge applied and Git dropped — never what
// someone else created, never a hook — dependents first.
func TestPruneOnlyWhatForgeApplied(t *testing.T) {
	c := newClusterApplier()
	s := newRun(c, live.Op{Kind: live.OpSync, Prune: true})
	roots := map[string]*root{
		"networking.k8s.io/Ingress/shop/old": {forge: true},
		"/ConfigMap/shop/old":                {forge: true},
		"/PersistentVolumeClaim/shop/data-0": {}, // a StatefulSet's claim: nobody applied it
		"batch/Job/shop/smoke-1":             {forge: true, hook: true},
		"/Service/shop/web":                  {forge: true},
	}
	s.w.rootsByApp["shop/web"] = map[string]bool{}
	for k, r := range roots {
		s.w.roots[k] = r
		s.w.rootsByApp["shop/web"][k] = true
	}
	res := s.execute(context.Background(), objects(t, `apiVersion: v1
kind: Service
metadata: {name: web, namespace: shop}`), "abc1234")
	if !res.OK || strings.Join(c.log, ",") != "apply Service/web,delete Ingress/old,delete ConfigMap/old" {
		t.Errorf("prune = %+v, log %v", res, c.log)
	}
	if res.Message != "Synced 1 resources to abc1234, pruned 2" {
		t.Errorf("message = %q", res.Message)
	}
}
