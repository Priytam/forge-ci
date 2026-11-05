package kube

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/priytamjeepandey/forge-ci/internal/live"
)

func TestCanaryReplicas(t *testing.T) {
	for _, tc := range []struct {
		stable int64
		weight int
		want   int64
	}{{4, 20, 1}, {4, 50, 4}, {10, 25, 3}, {2, 20, 1}, {1, 10, 1}, {0, 50, 1}} {
		if got := canaryReplicas(tc.stable, tc.weight); got != tc.want {
			t.Errorf("canaryReplicas(%d, %d) = %d, want %d", tc.stable, tc.weight, got, tc.want)
		}
	}
}

func TestRolloutSpecs(t *testing.T) {
	objs := objects(t, `apiVersion: forge.dev/v1
kind: Rollout
metadata: {name: web}
spec:
  steps: [{weight: 20}, {pause: {duration: 30s}}, {weight: 50}, {pause: {}}]
  analysis: {interval: 5s, http: {path: /healthz}}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: web, namespace: shop}`)
	rest, specs, err := rollouts(objs)
	if err != nil || len(rest) != 1 || specs["web"] == nil {
		t.Fatalf("rollouts = %v, %v, %v", rest, specs, err)
	}
	s := specs["web"]
	if len(s.Steps) != 4 || time.Duration(s.Steps[1].Pause.Duration) != 30*time.Second || s.Steps[3].Pause.Duration != 0 ||
		time.Duration(s.Analysis.Interval) != 5*time.Second || s.Analysis.FailureLimit != 2 || s.Analysis.HTTP.SuccessRate != 0.95 {
		t.Errorf("spec = %+v", s)
	}
	for _, bad := range []string{
		"spec: {steps: [{weight: 20, pause: {}}]}",
		"spec: {steps: [{weight: 120}]}",
		"spec: {analysis: {prometheus: {address: http://p}}}",
	} {
		if _, _, err := rollouts(objects(t, "apiVersion: forge.dev/v1\nkind: Rollout\nmetadata: {name: web}\n"+bad)); err == nil {
			t.Errorf("%s: accepted", bad)
		}
	}
}

const canaryApp = `apiVersion: forge.dev/v1
kind: Rollout
metadata: {name: web}
spec:
  steps: [{weight: 25}, {pause: {duration: 20ms}}, {weight: 50}, {pause: {duration: 20ms}}]
  analysis: {interval: 5ms}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: web, namespace: shop, labels: {app.kubernetes.io/name: web}}
spec:
  replicas: 4
  selector: {matchLabels: {app.kubernetes.io/name: web}}
  template:
    metadata: {labels: {app.kubernetes.io/name: web}}
    spec: {containers: [{name: web, image: "web:v2"}]}`

// canaryCluster runs the stable web:v1 Deployment; canary pods follow the
// canary's replicas with the health the test sets.
type canaryCluster struct {
	*clusterApplier
	mu       sync.Mutex
	podState func(i int) corev1.PodStatus
	podIP    string
	replicas []int64 // the canary's replicas, as applied
}

func newCanaryCluster(t *testing.T) *canaryCluster {
	c := &canaryCluster{clusterApplier: newClusterApplier()}
	stable := objects(t, strings.Replace(strings.SplitN(canaryApp, "---\n", 2)[1], "web:v2", "web:v1", 1))[0]
	c.objs["Deployment/web"] = stable
	c.podState = func(int) corev1.PodStatus {
		return corev1.PodStatus{Phase: corev1.PodRunning, PodIP: c.podIP, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}
	}
	return c
}

func (c *canaryCluster) Apply(ctx context.Context, o *unstructured.Unstructured, dryRun, force bool) (string, error) {
	if o.GetName() == "web-canary" {
		n, _, _ := unstructured.NestedInt64(o.Object, "spec", "replicas")
		c.mu.Lock()
		c.replicas = append(c.replicas, n)
		c.mu.Unlock()
	}
	return c.clusterApplier.Apply(ctx, o, dryRun, force)
}

func (c *canaryCluster) pods(_ context.Context, _, selector string) ([]corev1.Pod, error) {
	if !strings.Contains(selector, "forge.dev/track=canary") {
		return nil, fmt.Errorf("selector %q does not pick the canary", selector)
	}
	c.clusterApplier.mu.Lock()
	o := c.objs["Deployment/web-canary"]
	c.clusterApplier.mu.Unlock()
	if o == nil {
		return nil, nil
	}
	n, _, _ := unstructured.NestedInt64(o.Object, "spec", "replicas")
	var out []corev1.Pod
	for i := range int(n) {
		out = append(out, corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web-canary-" + strconv.Itoa(i)}, Status: c.podState(i)})
	}
	return out, nil
}

func runCanary(t *testing.T, c *canaryCluster, op live.Op) (live.OpResult, string, *Provider) {
	t.Helper()
	defer func(p time.Duration) { wavePoll = p }(wavePoll)
	wavePoll = time.Millisecond
	s := newRun(c.clusterApplier, op)
	s.applier = c
	s.pods = c.pods
	s.w.p = &Provider{}
	var out strings.Builder
	var mu sync.Mutex
	s.out = func(b []byte) { mu.Lock(); out.Write(b); mu.Unlock() }
	res := s.execute(context.Background(), objects(t, canaryApp), "abc1234")
	mu.Lock()
	defer mu.Unlock()
	return res, out.String(), s.w.p
}

func TestCanaryPromotes(t *testing.T) {
	c := newCanaryCluster(t)
	res, out, _ := runCanary(t, c, live.Op{Kind: live.OpSync})
	if !res.OK || len(res.Resources) != 1 || res.Resources[0].Action != "promoted" {
		t.Fatalf("result = %+v\n%s", res, out)
	}
	if fmt.Sprint(c.replicas) != "[1 4]" { // 25% and 50% of 4 stable pods
		t.Errorf("canary replicas %v", c.replicas)
	}
	img, _, _ := unstructured.NestedSlice(c.objs["Deployment/web"].Object, "spec", "template", "spec", "containers")
	if !strings.Contains(fmt.Sprint(img), "web:v2") || c.objs["Deployment/web-canary"] != nil {
		t.Errorf("after promotion: stable %v, canary left %v", img, c.objs["Deployment/web-canary"] != nil)
	}
	if !strings.Contains(res.Resources[0].Message, "25% → 50% → 100%") || !strings.Contains(out, "Analysis") {
		t.Errorf("message %q\n%s", res.Resources[0].Message, out)
	}
}

func TestCanaryAbortsOnAnalysis(t *testing.T) {
	c := newCanaryCluster(t)
	c.podState = func(int) corev1.PodStatus {
		return corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "web", RestartCount: 3,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}}}}
	}
	res, out, _ := runCanary(t, c, live.Op{Kind: live.OpSync})
	if res.OK || !res.Aborted || !strings.Contains(res.Message, "Rollout of web aborted: analysis failed at 25%: health: web-canary-0 web: CrashLoopBackOff") {
		t.Fatalf("result = %+v\n%s", res, out)
	}
	img, _, _ := unstructured.NestedSlice(c.objs["Deployment/web"].Object, "spec", "template", "spec", "containers")
	if !strings.Contains(fmt.Sprint(img), "web:v1") || c.objs["Deployment/web-canary"] != nil {
		t.Errorf("after abort: stable %v, canary left %v", img, c.objs["Deployment/web-canary"] != nil)
	}
}

func TestCanaryHTTPAnalysis(t *testing.T) {
	var hits int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		hits++
		bad := hits%2 == 0
		mu.Unlock()
		if bad {
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	c := newCanaryCluster(t)
	c.podIP = host
	app := strings.Replace(canaryApp, "analysis: {interval: 5ms}", "analysis: {interval: 5ms, http: {port: "+port+", path: /healthz}}", 1)
	defer func(p time.Duration) { wavePoll = p }(wavePoll)
	wavePoll = time.Millisecond
	s := newRun(c.clusterApplier, live.Op{Kind: live.OpSync})
	s.applier, s.pods, s.w.p = c, c.pods, &Provider{}
	res := s.execute(context.Background(), objects(t, app), "abc1234")
	if res.OK || !strings.Contains(res.Message, "http: 50% of 5 requests to /healthz succeeded, want 95% (last: HTTP 500)") &&
		!strings.Contains(res.Message, "of 5 requests to /healthz succeeded, want 95%") {
		t.Fatalf("result = %+v", res)
	}
}

func TestPrometheusQuery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if q := r.URL.Query().Get("query"); q != `rate(errors{deployment="web-canary",namespace="shop"}[1m])` {
			fmt.Fprintf(w, `{"status":"error","error":"unexpected query %s"}`, q)
			return
		}
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1730000000,"0.07"]}]}}`)
	}))
	defer srv.Close()
	v, err := promQuery(context.Background(), srv.Client(), &PromCheck{Address: srv.URL,
		Query: `rate(errors{deployment="{{canary}}",namespace="{{namespace}}"}[1m])`}, "web-canary", "shop")
	if err != nil || v != 0.07 {
		t.Errorf("promQuery = %v, %v", v, err)
	}
}

// Abort and promote reach a running rollout from another operation.
func TestCanaryControls(t *testing.T) {
	app := strings.Replace(canaryApp, "{pause: {duration: 20ms}}, {weight: 50}", "{pause: {}}, {weight: 50}", 1)
	for _, tc := range []struct {
		signal live.Op
		ok     bool
		want   string
	}{
		{live.Op{Kind: live.OpAbort}, false, "aborted by hand at 25%"},
		{live.Op{Kind: live.OpPromote}, true, "25% → 50% → 100%"},                  // resume the manual pause
		{live.Op{Kind: live.OpPromote, Full: true}, true, "then promoted by hand"}, // straight to 100%
	} {
		c := newCanaryCluster(t)
		defer func(p time.Duration) { wavePoll = p }(wavePoll)
		wavePoll = time.Millisecond
		s := newRun(c.clusterApplier, live.Op{Kind: live.OpSync})
		s.applier, s.pods, s.w.p = c, c.pods, &Provider{}
		done := make(chan live.OpResult)
		go func() { done <- s.execute(context.Background(), objects(t, app), "abc1234") }()
		tc.signal.App = live.App{Key: "shop/web"}
		deadline := time.Now().Add(5 * time.Second)
		for !s.w.p.signal(tc.signal) {
			if time.Now().After(deadline) {
				t.Fatal("the rollout never started")
			}
			time.Sleep(time.Millisecond)
		}
		res := <-done
		if res.OK != tc.ok || !strings.Contains(res.Message+fmt.Sprint(res.Resources), tc.want) {
			t.Errorf("%s (full %v): %+v", tc.signal.Kind, tc.signal.Full, res)
		}
	}
}
