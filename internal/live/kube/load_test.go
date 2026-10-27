package kube

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"os"
	goruntime "runtime"
	"runtime/debug"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/cache"

	"github.com/priytamjeepandey/forge-ci/internal/live"
)

// The agent's footprint budget (batch 9, §5.6): ≤150 MB and ≤0.2 vCPU while
// watching 100,000 objects. TestAgentFootprint builds a synthetic cluster —
// 2,000 apps of 50 objects (Deployment, two ReplicaSets, Service, ConfigMap,
// HPA, 44 pods), realistic pod manifests with managed fields — feeds it to
// the real provider through paged lists and watches, ships every delta as
// the agent does (JSON + gzip), and measures heap and CPU at steady state.
//
// `go test` runs 20,000 objects and checks the per-object cost scales to the
// budget; the full run (numbers in docs/live.md) is
//
//	LIVE_LOAD=100000 go test ./internal/live/kube -run TestAgentFootprint -v
const (
	perApp      = 50
	podsPerApp  = 44
	budgetHeap  = 150 << 20
	budgetVCPU  = 0.2
	churnPerSec = 200 // pod status updates per second at "steady state"
)

type synthetic struct {
	apps int
	mu   sync.Mutex
	ws   map[string]*watch.FakeWatcher
}

func (s *synthetic) meta(ns, name, uid string, owner metav1.Object, ownerKind string) metav1.ObjectMeta {
	m := metav1.ObjectMeta{
		Name: name, Namespace: ns, UID: types.UID(uid), ResourceVersion: "1", Generation: 1,
		CreationTimestamp: metav1.NewTime(time.Unix(1_790_000_000, 0)),
		Labels:            map[string]string{"app.kubernetes.io/name": name, "app.kubernetes.io/part-of": ns, "pod-template-hash": "7d9f8c6b5"},
		Annotations:       map[string]string{lastApplied: string(bytes.Repeat([]byte("x"), 900)), "prometheus.io/scrape": "true"},
		ManagedFields: []metav1.ManagedFieldsEntry{
			{Manager: "kube-controller-manager", Operation: "Update", APIVersion: "v1", FieldsType: "FieldsV1",
				FieldsV1: &metav1.FieldsV1{Raw: bytes.Repeat([]byte(`{"f:metadata":{}}`), 30)}},
			{Manager: "kubelet", Operation: "Update", APIVersion: "v1", FieldsType: "FieldsV1", Subresource: "status",
				FieldsV1: &metav1.FieldsV1{Raw: bytes.Repeat([]byte(`{"f:status":{}}`), 40)}},
		},
	}
	if owner != nil {
		yes := true
		m.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: ownerKind, Name: owner.GetName(), UID: owner.GetUID(), Controller: &yes}}
		delete(m.Labels, "app.kubernetes.io/part-of")
	}
	return m
}

func (s *synthetic) app(i int) (ns, name string) {
	return fmt.Sprintf("team-%02d", i%50), fmt.Sprintf("svc-%05d", i)
}

func (s *synthetic) pod(i, j, restarts int) *corev1.Pod {
	ns, name := s.app(i)
	rs := &metav1.ObjectMeta{Name: name + "-7d9f8c6b5", UID: types.UID(fmt.Sprintf("rs-%d", i))}
	ready := corev1.ConditionTrue
	return &corev1.Pod{
		ObjectMeta: s.meta(ns, fmt.Sprintf("%s-7d9f8c6b5-%05d", name, j), fmt.Sprintf("pod-%d-%d", i, j), rs, "ReplicaSet"),
		Spec: corev1.PodSpec{
			NodeName: fmt.Sprintf("node-%03d", j%200), ServiceAccountName: "default", RestartPolicy: corev1.RestartPolicyAlways,
			Containers: []corev1.Container{{
				Name: "app", Image: "registry.example.com/" + ns + "/" + name + ":v1.42.7",
				Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: 8080, Protocol: corev1.ProtocolTCP}},
				Env: []corev1.EnvVar{{Name: "APP_NAME", Value: name}, {Name: "TEAM", Value: ns}, {Name: "LOG_LEVEL", Value: "info"},
					{Name: "OTEL_EXPORTER_OTLP_ENDPOINT", Value: "http://otel-collector.observability:4317"}},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
					Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")}},
				ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromString("http")}}, PeriodSeconds: 10},
				VolumeMounts:   []corev1.VolumeMount{{Name: "kube-api-access-x7k2p", MountPath: "/var/run/secrets/kubernetes.io/serviceaccount", ReadOnly: true}},
			}},
			Volumes: []corev1.Volume{{Name: "kube-api-access-x7k2p", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
				Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token"}},
					{ConfigMap: &corev1.ConfigMapProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "kube-root-ca.crt"}}}}}}}},
			Tolerations: []corev1.Toleration{{Key: "node.kubernetes.io/not-ready", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning, PodIP: fmt.Sprintf("10.%d.%d.%d", i%250, j%250, restarts%250), HostIP: "172.18.0.4", QOSClass: corev1.PodQOSBurstable,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: ready}, {Type: corev1.ContainersReady, Status: ready},
				{Type: corev1.PodInitialized, Status: ready}, {Type: corev1.PodScheduled, Status: ready}},
			ContainerStatuses: []corev1.ContainerStatus{{Name: "app", Ready: true, RestartCount: int32(restarts),
				Image: "registry.example.com/" + ns + "/" + name + ":v1.42.7", ImageID: "registry.example.com/x@sha256:" + fmt.Sprintf("%064d", i),
				ContainerID: "containerd://" + fmt.Sprintf("%064d", j), State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}},
		},
	}
}

// objects returns item n of kind k (n < count(k)).
func (s *synthetic) object(k *kind, n int) runtime.Object {
	replicas := int32(podsPerApp)
	tmpl := corev1.PodTemplateSpec{Spec: s.pod(n, 0, 0).Spec}
	ns, name := s.app(n)
	dep := s.meta(ns, name, fmt.Sprintf("dep-%d", n), nil, "")
	switch k.name {
	case "Deployment":
		return &appsv1.Deployment{ObjectMeta: dep, Spec: appsv1.DeploymentSpec{Replicas: &replicas, Template: tmpl},
			Status: appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: replicas, UpdatedReplicas: replicas, ReadyReplicas: replicas, AvailableReplicas: replicas}}
	case "ReplicaSet":
		i, old := n/2, n%2 == 1
		ns, name := s.app(i)
		depOwner := s.meta(ns, name, fmt.Sprintf("dep-%d", i), nil, "")
		uid, r := fmt.Sprintf("rs-%d", i), replicas
		if old {
			uid, r = fmt.Sprintf("rs-old-%d", i), 0
		}
		return &appsv1.ReplicaSet{ObjectMeta: s.meta(ns, name+"-"+uid, uid, &depOwner, "Deployment"), Spec: appsv1.ReplicaSetSpec{Replicas: &r, Template: tmpl},
			Status: appsv1.ReplicaSetStatus{ObservedGeneration: 1, Replicas: r, ReadyReplicas: r, AvailableReplicas: r}}
	case "Pod":
		return s.pod(n/podsPerApp, n%podsPerApp, 0)
	case "Service":
		return &corev1.Service{ObjectMeta: s.meta(ns, name, fmt.Sprintf("svc-%d", n), nil, ""),
			Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, ClusterIP: "10.96.1.1", Ports: []corev1.ServicePort{{Port: 80, TargetPort: intstr.FromString("http")}}}}
	case "ConfigMap":
		m := s.meta(ns, name+"-config", fmt.Sprintf("cm-%d", n), nil, "")
		m.Labels["app.kubernetes.io/name"] = name
		return &corev1.ConfigMap{ObjectMeta: m,
			Data: map[string]string{"APP_NAME": name, "bundle.pem": string(bytes.Repeat([]byte("A"), 4096))}}
	case "HorizontalPodAutoscaler":
		return &autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: s.meta(ns, name, fmt.Sprintf("hpa-%d", n), nil, ""),
			Spec: autoscalingv2.HorizontalPodAutoscalerSpec{MaxReplicas: 60, ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: name}},
			Status: autoscalingv2.HorizontalPodAutoscalerStatus{CurrentReplicas: replicas, Conditions: []autoscalingv2.HorizontalPodAutoscalerCondition{
				{Type: autoscalingv2.ScalingActive, Status: corev1.ConditionTrue, Reason: "ValidMetricFound"}}}}
	}
	return nil
}

func (s *synthetic) count(k *kind) int {
	switch k.name {
	case "Pod":
		return s.apps * podsPerApp
	case "ReplicaSet":
		return s.apps * 2
	case "Deployment", "Service", "ConfigMap", "HorizontalPodAutoscaler":
		return s.apps
	}
	return 0
}

// listWatch pages through count(k) generated objects and hands out a fake
// watch the test drives.
func (s *synthetic) listWatch(k *kind) cache.ListerWatcher {
	return &cache.ListWatch{
		ListFunc: func(o metav1.ListOptions) (runtime.Object, error) {
			start, _ := strconv.Atoi(o.Continue)
			end := min(start+int(o.Limit), s.count(k))
			list := &metav1.List{ListMeta: metav1.ListMeta{ResourceVersion: "1"}}
			for n := start; n < end; n++ {
				list.Items = append(list.Items, runtime.RawExtension{Object: s.object(k, n)})
			}
			if end < s.count(k) {
				list.Continue = strconv.Itoa(end)
			}
			return list, nil
		},
		WatchFunc: func(metav1.ListOptions) (watch.Interface, error) {
			w := watch.NewFakeWithChanSize(1024, false)
			s.mu.Lock()
			s.ws[k.name] = w
			s.mu.Unlock()
			return w, nil
		},
	}
}

func cpuSeconds() float64 {
	var ru syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return float64(ru.Utime.Sec+ru.Stime.Sec) + float64(ru.Utime.Usec+ru.Stime.Usec)/1e6
}

func heapInuse() uint64 {
	goruntime.GC()
	var m goruntime.MemStats
	goruntime.ReadMemStats(&m)
	return m.HeapInuse
}

type counter struct{ n int64 }

func (c *counter) Write(p []byte) (int, error) {
	atomic.AddInt64(&c.n, int64(len(p)))
	return len(p), nil
}

// ship encodes deltas the way the agent does and counts the bytes.
func ship(ds []live.Delta, c *counter) {
	zw := gzip.NewWriter(c)
	_ = json.NewEncoder(zw).Encode(live.Batch{Deltas: ds})
	_ = zw.Close()
}

func TestAgentFootprint(t *testing.T) {
	objects, measure := 20_000, 3*time.Second
	if v, _ := strconv.Atoi(os.Getenv("LIVE_LOAD")); v > 0 {
		objects, measure = v, 10*time.Second
	}
	s := &synthetic{apps: objects / perApp, ws: map[string]*watch.FakeWatcher{}}
	// The agent runs with a soft memory limit (cmd/agent), so the GC works
	// harder before the heap reaches the budget; measure under the same.
	defer debug.SetMemoryLimit(debug.SetMemoryLimit(AgentMemoryLimit))
	base := heapInuse()

	p, err := New(nil, Config{})
	if err != nil {
		t.Fatal(err)
	}
	p.listWatch = s.listWatch
	tr := live.NewTracker(live.DefaultWindow)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var peak atomic.Uint64
	sample := time.NewTicker(25 * time.Millisecond)
	defer sample.Stop()
	go func() {
		var m goruntime.MemStats
		for range sample.C {
			goruntime.ReadMemStats(&m)
			if m.HeapInuse > peak.Load() {
				peak.Store(m.HeapInuse)
			}
		}
	}()

	start := time.Now()
	go func() { _ = p.Watch(ctx, tr) }()
	var initial counter
	for {
		ds, done := tr.Take(4 << 20)
		ship(ds, &initial)
		if done {
			break
		}
		if len(ds) == 0 {
			time.Sleep(10 * time.Millisecond)
		}
		if time.Since(start) > 5*time.Minute {
			t.Fatal("initial report did not complete")
		}
	}
	startup := time.Since(start)
	tracked, apps := tr.Counts()
	if tracked != objects-objects%perApp || apps != s.apps {
		t.Fatalf("tracked %d objects in %d apps, want %d", tracked, apps, objects)
	}
	steady := heapInuse() - base

	// Steady state: pods change (restarts, IPs) at churnPerSec; the agent
	// takes and ships deltas every window.
	var shipped counter
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		tick := time.NewTicker(time.Second / churnPerSec)
		defer tick.Stop()
		s.mu.Lock()
		pods := s.ws["Pod"]
		s.mu.Unlock()
		for n := 1; ; n++ {
			select {
			case <-stop:
				return
			case <-tick.C:
				i := (n * 7919) % s.apps
				pods.Modify(s.pod(i, n%podsPerApp, n))
			}
		}
	}()
	go func() {
		defer wg.Done()
		tick := time.NewTicker(live.DefaultWindow)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				ds, _ := tr.Take(4 << 20)
				ship(ds, &shipped)
			}
		}
	}()
	cpu0 := cpuSeconds()
	time.Sleep(measure)
	vcpu := (cpuSeconds() - cpu0) / measure.Seconds()
	close(stop)
	wg.Wait()
	bps := float64(shipped.n) / measure.Seconds()

	t.Logf("objects=%d apps=%d startup=%s initial-report=%.1f MB (gzip)", tracked, apps, startup.Round(time.Millisecond), float64(initial.n)/1e6)
	t.Logf("heap: steady %.1f MB (%.0f B/object), peak during startup %.1f MB", float64(steady)/(1<<20), float64(steady)/float64(tracked), float64(peak.Load()-base)/(1<<20))
	t.Logf("steady state, %d pod updates/s: %.3f vCPU (incl. generating the updates), %.0f B/s shipped = %.1f B/s per 1k resources",
		churnPerSec, vcpu, bps, bps/float64(tracked)*1000)

	// Per-object cost must scale to the budget at 100k objects.
	if perObj := float64(steady) / float64(tracked); perObj*100_000 > budgetHeap {
		t.Errorf("steady heap %.0f B/object → %.0f MB at 100k objects, budget 150 MB", perObj, perObj*100_000/(1<<20))
	}
	if objects >= 100_000 {
		if p := peak.Load() - base; p > budgetHeap {
			t.Errorf("startup peak heap %.1f MB over the 150 MB budget", float64(p)/(1<<20))
		}
		if vcpu > budgetVCPU {
			t.Errorf("steady CPU %.3f vCPU over the 0.2 budget", vcpu)
		}
	}
}
