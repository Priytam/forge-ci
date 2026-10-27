package kube

import (
	"encoding/json"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/priytamjeepandey/forge-ci/internal/live"
)

func i32(n int32) *int32 { return &n }

func running(ready bool, cs ...corev1.ContainerStatus) *corev1.Pod {
	st := corev1.ConditionFalse
	if ready {
		st = corev1.ConditionTrue
	}
	return &corev1.Pod{Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyAlways},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: st}}, ContainerStatuses: cs}}
}

func waiting(reason string) corev1.ContainerStatus {
	return corev1.ContainerStatus{Name: "app", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: "details"}}}
}

func TestHealthRules(t *testing.T) {
	healthy := appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: 2, UpdatedReplicas: 2, AvailableReplicas: 2}
	dep := func(mut func(*appsv1.Deployment)) *appsv1.Deployment {
		d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "cart", Generation: 1}, Spec: appsv1.DeploymentSpec{Replicas: i32(2)}, Status: healthy}
		mut(d)
		return d
	}
	now := metav1.Now()
	cases := []struct {
		name string
		got  *live.Health
		want string
		msg  string
	}{
		{"deployment healthy", deploymentHealth(dep(func(*appsv1.Deployment) {})), live.Healthy, ""},
		{"deployment paused", deploymentHealth(dep(func(d *appsv1.Deployment) { d.Spec.Paused = true })), live.Suspended, "paused"},
		{"deployment spec not observed", deploymentHealth(dep(func(d *appsv1.Deployment) { d.Generation = 2 })), live.Progressing, "generation"},
		{"deployment rolling", deploymentHealth(dep(func(d *appsv1.Deployment) { d.Status.UpdatedReplicas = 1 })), live.Progressing, "1 out of 2 new replicas"},
		{"deployment old replicas", deploymentHealth(dep(func(d *appsv1.Deployment) { d.Status.Replicas = 3 })), live.Progressing, "1 old replicas"},
		{"deployment unavailable", deploymentHealth(dep(func(d *appsv1.Deployment) { d.Status.AvailableReplicas = 0 })), live.Progressing, "0 of 2 updated"},
		{"deployment past deadline", deploymentHealth(dep(func(d *appsv1.Deployment) {
			d.Status.Conditions = []appsv1.DeploymentCondition{{Type: appsv1.DeploymentProgressing, Reason: "ProgressDeadlineExceeded"}}
		})), live.Degraded, "progress deadline"},

		{"replicaset available", replicaSetHealth(&appsv1.ReplicaSet{Spec: appsv1.ReplicaSetSpec{Replicas: i32(1)}, Status: appsv1.ReplicaSetStatus{AvailableReplicas: 1}}), live.Healthy, ""},
		{"replicaset waiting", replicaSetHealth(&appsv1.ReplicaSet{Spec: appsv1.ReplicaSetSpec{Replicas: i32(2)}, Status: appsv1.ReplicaSetStatus{AvailableReplicas: 1}}), live.Progressing, "1 out of 2"},
		{"replicaset failure", replicaSetHealth(&appsv1.ReplicaSet{Status: appsv1.ReplicaSetStatus{Conditions: []appsv1.ReplicaSetCondition{
			{Type: appsv1.ReplicaSetReplicaFailure, Status: corev1.ConditionTrue, Message: "quota exceeded"}}}}), live.Degraded, "quota exceeded"},

		{"pod ready", podHealth(running(true)), live.Healthy, ""},
		{"pod crash loop", podHealth(running(false, waiting("CrashLoopBackOff"))), live.Degraded, "app: CrashLoopBackOff: details"},
		{"pod image pull", podHealth(&corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{waiting("ImagePullBackOff")}}}), live.Degraded, "ImagePullBackOff"},
		{"pod init crash", podHealth(&corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending, InitContainerStatuses: []corev1.ContainerStatus{waiting("CrashLoopBackOff")}}}), live.Degraded, "CrashLoopBackOff"},
		{"pod creating", podHealth(&corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{waiting("ContainerCreating")}}}), live.Progressing, ""},
		{"pod not ready yet", podHealth(running(false)), live.Progressing, "not ready"},
		{"pod restarted and not ready", podHealth(running(false, corev1.ContainerStatus{Name: "app",
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled"}}})), live.Degraded, "oomkilled"},
		{"pod job running", podHealth(&corev1.Pod{Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}), live.Progressing, ""},
		{"pod succeeded", podHealth(&corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodSucceeded}}), live.Healthy, ""},
		{"pod failed", podHealth(&corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodFailed, ContainerStatuses: []corev1.ContainerStatus{{Name: "app",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 2}}}}}}), live.Degraded, "exit code 2"},
		{"pod terminating", podHealth(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &now}}), live.Progressing, "terminating"},

		{"statefulset ready", statefulSetHealth(&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Generation: 1}, Spec: appsv1.StatefulSetSpec{Replicas: i32(2)},
			Status: appsv1.StatefulSetStatus{ObservedGeneration: 1, ReadyReplicas: 2, CurrentRevision: "r1", UpdateRevision: "r1"}}), live.Healthy, ""},
		{"statefulset rolling", statefulSetHealth(&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Generation: 1}, Spec: appsv1.StatefulSetSpec{Replicas: i32(2)},
			Status: appsv1.StatefulSetStatus{ObservedGeneration: 1, ReadyReplicas: 2, CurrentRevision: "r1", UpdateRevision: "r2"}}), live.Progressing, "rolling update"},
		{"statefulset partition", statefulSetHealth(&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Generation: 1}, Spec: appsv1.StatefulSetSpec{Replicas: i32(3),
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{Type: appsv1.RollingUpdateStatefulSetStrategyType, RollingUpdate: &appsv1.RollingUpdateStatefulSetStrategy{Partition: i32(1)}}},
			Status: appsv1.StatefulSetStatus{ObservedGeneration: 1, ReadyReplicas: 3, UpdatedReplicas: 1}}), live.Progressing, "partitioned"},
		{"daemonset rolling", daemonSetHealth(&appsv1.DaemonSet{Status: appsv1.DaemonSetStatus{DesiredNumberScheduled: 3, UpdatedNumberScheduled: 2}}), live.Progressing, "2 out of 3"},
		{"daemonset ready", daemonSetHealth(&appsv1.DaemonSet{Status: appsv1.DaemonSetStatus{DesiredNumberScheduled: 3, UpdatedNumberScheduled: 3, NumberAvailable: 3}}), live.Healthy, ""},

		{"service", serviceHealth(&corev1.Service{}), live.Healthy, ""},
		{"service waiting for lb", serviceHealth(&corev1.Service{Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer}}), live.Progressing, "load balancer"},
		{"ingress with address", ingressHealth(&networkingv1.Ingress{Status: networkingv1.IngressStatus{LoadBalancer: networkingv1.IngressLoadBalancerStatus{
			Ingress: []networkingv1.IngressLoadBalancerIngress{{Hostname: "localhost"}}}}}), live.Healthy, ""},
		{"ingress without address", ingressHealth(&networkingv1.Ingress{}), live.Progressing, "address"},

		{"job complete", jobHealth(&batchv1.Job{Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}}}), live.Healthy, ""},
		{"job failed", jobHealth(&batchv1.Job{Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Message: "BackoffLimitExceeded"}}}}), live.Degraded, "BackoffLimit"},
		{"job suspended", jobHealth(&batchv1.Job{Spec: batchv1.JobSpec{Suspend: func() *bool { b := true; return &b }()}}), live.Suspended, ""},
		{"job running", jobHealth(&batchv1.Job{}), live.Progressing, ""},

		{"hpa scaling", hpaHealth(&autoscalingv2.HorizontalPodAutoscaler{Status: autoscalingv2.HorizontalPodAutoscalerStatus{Conditions: []autoscalingv2.HorizontalPodAutoscalerCondition{
			{Type: autoscalingv2.AbleToScale, Status: corev1.ConditionTrue}, {Type: autoscalingv2.ScalingActive, Status: corev1.ConditionTrue, Message: "valid metric"}}}}), live.Healthy, ""},
		{"hpa no metrics", hpaHealth(&autoscalingv2.HorizontalPodAutoscaler{Status: autoscalingv2.HorizontalPodAutoscalerStatus{Conditions: []autoscalingv2.HorizontalPodAutoscalerCondition{
			{Type: autoscalingv2.ScalingActive, Status: corev1.ConditionFalse, Reason: "FailedGetResourceMetric", Message: "no metrics"}}}}), live.Degraded, "no metrics"},
		{"hpa starting", hpaHealth(&autoscalingv2.HorizontalPodAutoscaler{}), live.Progressing, ""},

		{"pvc bound", pvcHealth(&corev1.PersistentVolumeClaim{Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound}}), live.Healthy, ""},
		{"pvc pending", pvcHealth(&corev1.PersistentVolumeClaim{Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending}}), live.Progressing, ""},
		{"pvc lost", pvcHealth(&corev1.PersistentVolumeClaim{Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimLost}}), live.Degraded, ""},
	}
	for _, c := range cases {
		if c.got.Status != c.want || !strings.Contains(strings.ToLower(c.got.Message), strings.ToLower(c.msg)) {
			t.Errorf("%s: got %+v, want %s containing %q", c.name, *c.got, c.want, c.msg)
		}
	}
}

func TestConvertTrimsManifests(t *testing.T) {
	k := kinds[7] // ConfigMap
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "cart-config", Namespace: "checkout", UID: "cm1",
			Annotations:   map[string]string{lastApplied: "{...}", "team": "checkout"},
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "kubectl"}}},
		Data: map[string]string{"small": "ok", "bundle.pem": strings.Repeat("A", 4000)},
	}
	_, r, ok := convert(k, cm)
	if !ok {
		t.Fatal("not converted")
	}
	var m struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Metadata   struct {
			Annotations   map[string]string `json:"annotations"`
			ManagedFields []any             `json:"managedFields"`
		} `json:"metadata"`
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(r.Manifest, &m); err != nil {
		t.Fatal(err)
	}
	if m.APIVersion != "v1" || m.Kind != "ConfigMap" || m.Metadata.ManagedFields != nil || m.Metadata.Annotations[lastApplied] != "" ||
		m.Metadata.Annotations["team"] != "checkout" || m.Data["small"] != "ok" || m.Data["bundle.pem"] != "(4000 bytes, not shown)" {
		t.Errorf("manifest not trimmed right: %s", r.Manifest)
	}
	if r.Health != nil || r.Summary.Info[0].Value != "2" {
		t.Errorf("configmap: health %+v, info %+v", r.Health, r.Summary.Info)
	}
}

// recorder is a sink that remembers the last state of each resource.
type recorder struct {
	res    map[string]live.Resource
	app    map[string]string
	events []live.Event
}

func newRecorder() *recorder {
	return &recorder{res: map[string]live.Resource{}, app: map[string]string{}}
}
func (r *recorder) Upsert(a live.App, res live.Resource) {
	r.res[res.UID], r.app[res.UID] = res, a.Key
}
func (r *recorder) Delete(_ live.App, uid string)  { delete(r.res, uid); delete(r.app, uid) }
func (r *recorder) Event(_ live.App, e live.Event) { r.events = append(r.events, e) }
func (r *recorder) Synced()                        {}
func (r *recorder) Desired(live.App, live.Desired) {}
func (r *recorder) SetSync(_ live.App, uid, sync string) {
	if res, ok := r.res[uid]; ok {
		res.Sync = sync
		r.res[uid] = res
	}
}

func newWatcher(t *testing.T, cfg Config) (*watcher, *recorder) {
	p, err := New(nil, cfg)
	if err != nil {
		t.Fatal(err)
	}
	rec := newRecorder()
	return &watcher{p: p, sink: rec, nodes: map[string]*node{}, apps: map[string]*live.App{},
		strs: map[string]string{}, healths: map[live.Health]*live.Health{},
		parked: map[string]map[string]parkedChild{}, parkedBy: map[string]string{}, restart: make(chan struct{})}, rec
}

func kindNamed(name string) *kind {
	for _, k := range kinds {
		if k.name == name {
			return k
		}
	}
	return nil
}

func owned(name, uid, ownerKind, ownerUID string, lbl map[string]string) metav1.ObjectMeta {
	yes := true
	m := metav1.ObjectMeta{Name: name, Namespace: "checkout", UID: types.UID(uid), Labels: lbl}
	if ownerUID != "" {
		m.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: ownerKind, UID: types.UID(ownerUID), Controller: &yes}}
	}
	return m
}

var appLabels = map[string]string{"app.kubernetes.io/name": "cart-api", "app.kubernetes.io/part-of": "checkout"}

// Watches of different kinds are not ordered: a pod can arrive before its
// ReplicaSet, which can arrive before its Deployment. Children wait for
// their owner and join its application when it shows up.
func TestGroupingChildrenBeforeOwners(t *testing.T) {
	w, rec := newWatcher(t, Config{})
	w.apply(kindNamed("Pod"), &corev1.Pod{ObjectMeta: owned("cart-api-x-1", "pod1", "ReplicaSet", "rs1", map[string]string{"app": "cart-api"})})
	w.apply(kindNamed("ReplicaSet"), &appsv1.ReplicaSet{ObjectMeta: owned("cart-api-x", "rs1", "Deployment", "dep1", nil)})
	if len(rec.res) != 0 {
		t.Fatalf("reported before the owner chain reached a labelled root: %v", rec.app)
	}
	w.apply(kindNamed("Deployment"), &appsv1.Deployment{ObjectMeta: owned("cart-api", "dep1", "", "", appLabels)})
	w.apply(kindNamed("Service"), &corev1.Service{ObjectMeta: owned("cart-api", "svc1", "", "", appLabels)})
	w.apply(kindNamed("Service"), &corev1.Service{ObjectMeta: owned("kubernetes", "svc0", "", "", nil)})
	want := map[string]string{"dep1": "", "rs1": "dep1", "pod1": "rs1", "svc1": ""}
	if len(rec.res) != len(want) {
		t.Fatalf("reported %v, want %v", rec.app, want)
	}
	for uid, parent := range want {
		r := rec.res[uid]
		if rec.app[uid] != "checkout/cart-api" || strings.Join(r.Parents, "") != parent {
			t.Errorf("%s: app %q parents %v, want checkout/cart-api under %q", uid, rec.app[uid], r.Parents, parent)
		}
	}
	if len(w.parked) != 0 || len(w.parkedBy) != 0 {
		t.Errorf("children left waiting: %v", w.parkedBy)
	}
	// Deleting the pod reports it; events about known resources are kept.
	w.apply(eventKind, &corev1.Event{ObjectMeta: metav1.ObjectMeta{UID: "e1"}, InvolvedObject: corev1.ObjectReference{UID: "pod1", Kind: "Pod"}, Type: "Warning", Reason: "BackOff"})
	w.remove("pod1")
	if _, ok := rec.res["pod1"]; ok || len(rec.events) != 1 {
		t.Errorf("after delete: %v, events %v", rec.app, rec.events)
	}
}

func TestGroupingRootSelectorAndRestart(t *testing.T) {
	w, rec := newWatcher(t, Config{RootSelector: "app.kubernetes.io/part-of=checkout"})
	w.apply(kindNamed("Deployment"), &appsv1.Deployment{ObjectMeta: owned("argocd-server", "a1", "", "", map[string]string{
		"app.kubernetes.io/name": "argocd-server", "app.kubernetes.io/part-of": "argocd"})})
	if len(rec.res) != 0 {
		t.Fatalf("root selector ignored: %v", rec.app)
	}
	w.apply(kindNamed("Deployment"), &appsv1.Deployment{ObjectMeta: owned("cart-api", "dep1", "", "", appLabels)})
	w.apply(kindNamed("ReplicaSet"), &appsv1.ReplicaSet{ObjectMeta: owned("cart-api-x", "rs1", "Deployment", "dep1", nil)})
	// Relabelling a top-level object moves its subtree: the provider asks
	// for a fresh report rather than patching children one by one.
	moved := map[string]string{"app.kubernetes.io/name": "basket", "app.kubernetes.io/part-of": "checkout"}
	w.apply(kindNamed("Deployment"), &appsv1.Deployment{ObjectMeta: owned("cart-api", "dep1", "", "", moved)})
	select {
	case <-w.restart:
	default:
		t.Error("an owner changed applications without asking for a fresh report")
	}
	if rec.app["dep1"] != "checkout/basket" {
		t.Errorf("deployment app = %q", rec.app["dep1"])
	}
}
