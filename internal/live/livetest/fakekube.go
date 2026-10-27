package livetest

import (
	"context"
	"fmt"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/priytamjeepandey/forge-ci/internal/live"
	"github.com/priytamjeepandey/forge-ci/internal/live/kube"
)

// FakeKube is a Kubernetes API (client-go's fake clientset) holding
// applications laid out the way the lab is: a labelled Deployment owning a
// ReplicaSet owning pods, plus a Service and a ConfigMap.
type FakeKube struct {
	Client *fake.Clientset
	ns     string
	n      int
}

// NewFakeKube returns an empty cluster.
func NewFakeKube(t *testing.T) *FakeKube {
	return &FakeKube{Client: fake.NewClientset(), ns: "team"}
}

func (f *FakeKube) Provider(t *testing.T) live.Provider {
	p, err := kube.New(f.Client, kube.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (f *FakeKube) uid(name string) types.UID {
	f.n++
	return types.UID(fmt.Sprintf("%s-%04d", name, f.n))
}

func ctrl(kind, name string, uid types.UID) []metav1.OwnerReference {
	yes := true
	return []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: kind, Name: name, UID: uid, Controller: &yes}}
}

// Deploy creates name's Deployment → ReplicaSet → 2 pods, Service and
// ConfigMap, in the given health.
func (f *FakeKube) Deploy(t *testing.T, name, health string) string {
	ctx := context.Background()
	replicas := int32(2)
	lbl := map[string]string{"app.kubernetes.io/name": name, "app.kubernetes.io/part-of": "team"}
	now := metav1.NewTime(time.Now().Add(-time.Hour))
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.ns, UID: f.uid(name), Labels: lbl, Generation: 1, CreationTimestamp: now},
		Spec: appsv1.DeploymentSpec{Replicas: &replicas, Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "app", Image: "registry/" + name + ":v1"}}}}},
		Status: appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: 2, UpdatedReplicas: 2, ReadyReplicas: 2, AvailableReplicas: 2},
	}
	switch health {
	case live.Progressing:
		d.Status.UpdatedReplicas, d.Status.AvailableReplicas, d.Status.ReadyReplicas = 1, 1, 1
	case live.Degraded:
		d.Status.AvailableReplicas, d.Status.ReadyReplicas = 0, 0
		d.Status.Conditions = []appsv1.DeploymentCondition{{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded"}}
	}
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-7d9f", Namespace: f.ns, UID: f.uid(name + "-rs"), Generation: 1,
			Labels: map[string]string{"app": name}, OwnerReferences: ctrl("Deployment", name, d.UID), CreationTimestamp: now},
		Spec:   appsv1.ReplicaSetSpec{Replicas: &replicas, Template: d.Spec.Template},
		Status: appsv1.ReplicaSetStatus{ObservedGeneration: 1, Replicas: 2, ReadyReplicas: d.Status.ReadyReplicas, AvailableReplicas: d.Status.AvailableReplicas},
	}
	mustCreate(t, func() error {
		_, err := f.Client.AppsV1().Deployments(f.ns).Create(ctx, d, metav1.CreateOptions{})
		return err
	})
	mustCreate(t, func() error {
		_, err := f.Client.AppsV1().ReplicaSets(f.ns).Create(ctx, rs, metav1.CreateOptions{})
		return err
	})
	for i := 0; i < 2; i++ {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-7d9f-%d", name, i), Namespace: f.ns, UID: f.uid(name + "-pod"),
				Labels: map[string]string{"app": name}, OwnerReferences: ctrl("ReplicaSet", rs.Name, rs.UID), CreationTimestamp: now},
			Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyAlways, NodeName: "node-1", Containers: d.Spec.Template.Spec.Containers},
			Status: corev1.PodStatus{Phase: corev1.PodRunning,
				Conditions:        []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
				ContainerStatuses: []corev1.ContainerStatus{{Name: "app", Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}},
		}
		if health == live.Degraded || (health == live.Progressing && i == 1) {
			pod.Status.Conditions[0].Status = corev1.ConditionFalse
			pod.Status.ContainerStatuses[0].Ready = false
			pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}
		}
		if health == live.Degraded {
			pod.Status.ContainerStatuses[0].RestartCount = 7
			pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
				Reason: "CrashLoopBackOff", Message: "back-off 5m0s restarting failed container"}}
		}
		mustCreate(t, func() error {
			_, err := f.Client.CoreV1().Pods(f.ns).Create(ctx, pod, metav1.CreateOptions{})
			return err
		})
		if health == live.Degraded && i == 0 {
			ev := &corev1.Event{
				ObjectMeta:     metav1.ObjectMeta{Name: pod.Name + ".backoff", Namespace: f.ns, UID: f.uid("event")},
				InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: pod.Name, Namespace: f.ns, UID: pod.UID},
				Type:           corev1.EventTypeWarning, Reason: "BackOff", Message: "Back-off restarting failed container app",
				Count: 12, FirstTimestamp: now, LastTimestamp: metav1.Now(),
			}
			mustCreate(t, func() error {
				_, err := f.Client.CoreV1().Events(f.ns).Create(ctx, ev, metav1.CreateOptions{})
				return err
			})
		}
	}
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: f.ns, UID: f.uid(name + "-svc"), Labels: lbl, CreationTimestamp: now},
		Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP, ClusterIP: "10.96.0.10"}}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name + "-config", Namespace: f.ns, UID: f.uid(name + "-cm"), Labels: lbl, CreationTimestamp: now},
		Data: map[string]string{"GREETING": "hello"}}
	mustCreate(t, func() error {
		_, err := f.Client.CoreV1().Services(f.ns).Create(ctx, svc, metav1.CreateOptions{})
		return err
	})
	mustCreate(t, func() error {
		_, err := f.Client.CoreV1().ConfigMaps(f.ns).Create(ctx, cm, metav1.CreateOptions{})
		return err
	})
	return f.ns + "/" + name
}

// Change scales the app's Deployment to 3 (one resource changes).
func (f *FakeKube) Change(t *testing.T, key string) {
	d, err := f.Client.AppsV1().Deployments(f.ns).Get(context.Background(), key[len(f.ns)+1:], metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	three := int32(3)
	d.Spec.Replicas = &three
	if _, err := f.Client.AppsV1().Deployments(f.ns).Update(context.Background(), d, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// Remove deletes everything of the app (as the garbage collector would).
func (f *FakeKube) Remove(t *testing.T, key string) {
	ctx, name := context.Background(), key[len(f.ns)+1:]
	c := f.Client
	pods, _ := c.CoreV1().Pods(f.ns).List(ctx, metav1.ListOptions{})
	for _, p := range pods.Items {
		if p.Labels["app"] == name {
			_ = c.CoreV1().Pods(f.ns).Delete(ctx, p.Name, metav1.DeleteOptions{})
		}
	}
	_ = c.AppsV1().ReplicaSets(f.ns).Delete(ctx, name+"-7d9f", metav1.DeleteOptions{})
	_ = c.AppsV1().Deployments(f.ns).Delete(ctx, name, metav1.DeleteOptions{})
	_ = c.CoreV1().Services(f.ns).Delete(ctx, name, metav1.DeleteOptions{})
	_ = c.CoreV1().ConfigMaps(f.ns).Delete(ctx, name+"-config", metav1.DeleteOptions{})
}

func mustCreate(t *testing.T, create func() error) {
	t.Helper()
	if err := create(); err != nil {
		t.Fatal(err)
	}
}
