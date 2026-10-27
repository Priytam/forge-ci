package kube

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/priytamjeepandey/forge-ci/internal/live"
)

// maxDataValue is the largest ConfigMap value kept in a manifest; bigger
// values are replaced by their size (certificates, dashboards, bundles).
const maxDataValue = 1024

const lastApplied = "kubectl.kubernetes.io/last-applied-configuration"

// trim drops what Forge never shows from obj, in place: managed fields, the
// last-applied annotation and large data values. It runs before an object is
// held anywhere, so even the reflector's initial list carries less.
func trim(obj any) (any, error) {
	if m, ok := obj.(metav1.Object); ok {
		m.SetManagedFields(nil)
		if a := m.GetAnnotations(); a[lastApplied] != "" {
			delete(a, lastApplied)
			m.SetAnnotations(a)
		}
	}
	if cm, ok := obj.(*corev1.ConfigMap); ok {
		for k, v := range cm.Data {
			if len(v) > maxDataValue {
				cm.Data[k] = fmt.Sprintf("(%d bytes, not shown)", len(v))
			}
		}
		for k, v := range cm.BinaryData {
			cm.BinaryData[k] = []byte(fmt.Sprintf("(%d bytes, not shown)", len(v)))
		}
	}
	return obj, nil
}

// convert projects a watched object onto Forge's model, manifest included.
// It returns ok=false for objects of kinds it does not know.
func convert(k *kind, obj any) (meta metav1.Object, r live.Resource, ok bool) {
	if _, err := trim(obj); err != nil {
		return nil, r, false
	}
	meta, ok = obj.(metav1.Object)
	if !ok {
		return nil, r, false
	}
	r = live.Resource{
		Group: k.group, Version: k.version, Kind: k.name,
		Namespace: meta.GetNamespace(), Name: meta.GetName(), UID: string(meta.GetUID()),
		CreatedAt: meta.GetCreationTimestamp().Time.UTC(),
	}
	switch o := obj.(type) {
	case *appsv1.Deployment:
		r.Health = deploymentHealth(o)
		r.Summary = live.Summary{Ready: ratio(o.Status.ReadyReplicas, o.Spec.Replicas), Images: images(o.Spec.Template.Spec)}
		r.Summary.Info = facts("Strategy", string(o.Spec.Strategy.Type), "Revision", o.Annotations["deployment.kubernetes.io/revision"],
			"Pod labels", labelString(o.Spec.Template.Labels))
	case *appsv1.ReplicaSet:
		r.Health = replicaSetHealth(o)
		r.Summary = live.Summary{Ready: ratio(o.Status.ReadyReplicas, o.Spec.Replicas), Images: images(o.Spec.Template.Spec)}
		r.Summary.Info = facts("Revision", o.Annotations["deployment.kubernetes.io/revision"])
	case *appsv1.StatefulSet:
		r.Health = statefulSetHealth(o)
		r.Summary = live.Summary{Ready: ratio(o.Status.ReadyReplicas, o.Spec.Replicas), Images: images(o.Spec.Template.Spec)}
		r.Summary.Info = facts("Service", o.Spec.ServiceName, "Strategy", string(o.Spec.UpdateStrategy.Type),
			"Pod labels", labelString(o.Spec.Template.Labels))
	case *appsv1.DaemonSet:
		r.Health = daemonSetHealth(o)
		r.Summary = live.Summary{Ready: fmt.Sprintf("%d/%d", o.Status.NumberReady, o.Status.DesiredNumberScheduled), Images: images(o.Spec.Template.Spec),
			Info: facts("Pod labels", labelString(o.Spec.Template.Labels))}
	case *corev1.Pod:
		r.Health = podHealth(o)
		r.Summary = podSummary(o)
	case *corev1.Service:
		r.Health = serviceHealth(o)
		r.Summary.Info = facts("Type", string(o.Spec.Type), "Cluster IP", o.Spec.ClusterIP, "Ports", servicePorts(o),
			"Selector", labelString(o.Spec.Selector))
	case *networkingv1.Ingress:
		r.Health = ingressHealth(o)
		r.Summary.Info = facts("Hosts", ingressHosts(o), "Address", lbAddress(o.Status.LoadBalancer.Ingress), "Class", deref(o.Spec.IngressClassName),
			"Backends", ingressBackends(o))
	case *batchv1.Job:
		r.Health = jobHealth(o)
		r.Summary = live.Summary{Ready: ratio(o.Status.Succeeded, o.Spec.Completions), Images: images(o.Spec.Template.Spec)}
	case *autoscalingv2.HorizontalPodAutoscaler:
		r.Health = hpaHealth(o)
		r.Summary.Info = facts("Target", o.Spec.ScaleTargetRef.Kind+"/"+o.Spec.ScaleTargetRef.Name,
			"Replicas", fmt.Sprintf("%d (min %d, max %d)", o.Status.CurrentReplicas, deref(o.Spec.MinReplicas), o.Spec.MaxReplicas))
	case *corev1.PersistentVolumeClaim:
		r.Health = pvcHealth(o)
		capacity := ""
		if q, ok := o.Status.Capacity[corev1.ResourceStorage]; ok {
			capacity = q.String()
		}
		r.Summary.Info = facts("Phase", string(o.Status.Phase), "Capacity", capacity, "Storage class", deref(o.Spec.StorageClassName))
	case *corev1.ConfigMap:
		r.Summary.Info = facts("Keys", strconv.Itoa(len(o.Data)+len(o.BinaryData)))
	default:
		return nil, r, false
	}
	// Typed objects come without apiVersion/kind; the manifest should say.
	if ro, isRO := obj.(runtime.Object); isRO {
		apiVersion := k.version
		if k.group != "" {
			apiVersion = k.group + "/" + k.version
		}
		ro.GetObjectKind().SetGroupVersionKind(schema.FromAPIVersionAndKind(apiVersion, k.name))
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return nil, r, false
	}
	r.Manifest = b
	return meta, r, true
}

func podSummary(p *corev1.Pod) live.Summary {
	s := live.Summary{Status: string(p.Status.Phase), Images: images(p.Spec)}
	ready := 0
	for _, c := range p.Status.ContainerStatuses {
		s.Restarts += int(c.RestartCount)
		if c.Ready {
			ready++
		}
		// kubectl's STATUS column: the first container that is stuck says why.
		if w := c.State.Waiting; w != nil && w.Reason != "" && s.Status == string(p.Status.Phase) {
			s.Status = w.Reason
		} else if t := c.State.Terminated; t != nil && t.Reason != "" && s.Status == string(p.Status.Phase) {
			s.Status = t.Reason
		}
	}
	if p.DeletionTimestamp != nil {
		s.Status = "Terminating"
	}
	s.Ready = fmt.Sprintf("%d/%d", ready, len(p.Spec.Containers))
	s.Info = facts("Node", p.Spec.NodeName, "Pod IP", p.Status.PodIP)
	return s
}

func images(spec corev1.PodSpec) []string {
	out := make([]string, 0, len(spec.Containers))
	for _, c := range spec.Containers {
		out = append(out, c.Image)
	}
	return out
}

// facts builds a summary's key/value list from pairs, skipping empty values.
func facts(pairs ...string) []live.KV {
	var out []live.KV
	for i := 0; i+1 < len(pairs); i += 2 {
		if pairs[i+1] != "" {
			out = append(out, live.KV{Name: pairs[i], Value: pairs[i+1]})
		}
	}
	return out
}

func ratio(have int32, want *int32) string {
	w := int32(1)
	if want != nil {
		w = *want
	}
	return fmt.Sprintf("%d/%d", have, w)
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

func servicePorts(s *corev1.Service) string {
	ps := make([]string, 0, len(s.Spec.Ports))
	for _, p := range s.Spec.Ports {
		ps = append(ps, fmt.Sprintf("%d→%s/%s", p.Port, p.TargetPort.String(), p.Protocol))
	}
	return strings.Join(ps, ", ")
}

func ingressHosts(i *networkingv1.Ingress) string {
	hosts := []string{}
	for _, r := range i.Spec.Rules {
		if r.Host != "" {
			hosts = append(hosts, r.Host)
		}
	}
	sort.Strings(hosts)
	return strings.Join(hosts, ", ")
}

func lbAddress(ing []networkingv1.IngressLoadBalancerIngress) string {
	out := []string{}
	for _, a := range ing {
		if a.Hostname != "" {
			out = append(out, a.Hostname)
		} else if a.IP != "" {
			out = append(out, a.IP)
		}
	}
	return strings.Join(out, ", ")
}

// event projects a Kubernetes event; ok=false for events without an object.
func event(e *corev1.Event) (live.Event, bool) {
	if e.InvolvedObject.UID == "" {
		return live.Event{}, false
	}
	first, last := e.FirstTimestamp.Time, e.LastTimestamp.Time
	if first.IsZero() {
		first = e.EventTime.Time
	}
	if last.IsZero() {
		last = first
	}
	count := int(e.Count)
	if e.Series != nil {
		count = int(e.Series.Count)
		last = e.Series.LastObservedTime.Time
	}
	if count == 0 {
		count = 1
	}
	return live.Event{
		UID: string(e.UID), ResourceUID: string(e.InvolvedObject.UID), Kind: e.InvolvedObject.Kind, Name: e.InvolvedObject.Name,
		Type: e.Type, Reason: e.Reason, Message: e.Message, Count: count, FirstSeen: first.UTC(), LastSeen: last.UTC(),
	}, true
}

// labelString renders labels as a selector does ("app=cart,tier=web"),
// sorted, so the Network view can match Services to workloads.
func labelString(l map[string]string) string {
	parts := make([]string, 0, len(l))
	for k, v := range l {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

// ingressBackends lists the Services an Ingress routes to.
func ingressBackends(i *networkingv1.Ingress) string {
	seen := map[string]bool{}
	add := func(b *networkingv1.IngressBackend) {
		if b != nil && b.Service != nil {
			seen[b.Service.Name] = true
		}
	}
	add(i.Spec.DefaultBackend)
	for _, r := range i.Spec.Rules {
		if r.HTTP == nil {
			continue
		}
		for _, p := range r.HTTP.Paths {
			add(&p.Backend)
		}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}
