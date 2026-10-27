package kube

import (
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"

	"github.com/priytamjeepandey/forge-ci/internal/live"
)

// Health rules per kind. They follow the semantics people know from Argo CD
// (a rollout is Progressing until every new replica is available, a
// Deployment past its progress deadline is Degraded), so the two tools agree
// on the same cluster. Kinds without a rule (ConfigMap) have no health.

func h(status, format string, a ...any) *live.Health {
	return &live.Health{Status: status, Message: fmt.Sprintf(format, a...)}
}

func deploymentHealth(d *appsv1.Deployment) *live.Health {
	if d.Spec.Paused {
		return h(live.Suspended, "Deployment is paused")
	}
	if d.Generation > d.Status.ObservedGeneration {
		return h(live.Progressing, "Waiting for rollout to finish: observed deployment generation less than desired generation")
	}
	for _, c := range d.Status.Conditions {
		if c.Type == appsv1.DeploymentProgressing && c.Reason == "ProgressDeadlineExceeded" {
			return h(live.Degraded, "Deployment %q exceeded its progress deadline", d.Name)
		}
	}
	st := d.Status
	switch {
	case d.Spec.Replicas != nil && st.UpdatedReplicas < *d.Spec.Replicas:
		return h(live.Progressing, "Waiting for rollout to finish: %d out of %d new replicas have been updated...", st.UpdatedReplicas, *d.Spec.Replicas)
	case st.Replicas > st.UpdatedReplicas:
		return h(live.Progressing, "Waiting for rollout to finish: %d old replicas are pending termination...", st.Replicas-st.UpdatedReplicas)
	case st.AvailableReplicas < st.UpdatedReplicas:
		return h(live.Progressing, "Waiting for rollout to finish: %d of %d updated replicas are available...", st.AvailableReplicas, st.UpdatedReplicas)
	}
	return h(live.Healthy, "")
}

func replicaSetHealth(rs *appsv1.ReplicaSet) *live.Health {
	if rs.Generation > rs.Status.ObservedGeneration {
		return h(live.Progressing, "Waiting for rollout to finish: observed replica set generation less than desired generation")
	}
	for _, c := range rs.Status.Conditions {
		if c.Type == appsv1.ReplicaSetReplicaFailure && c.Status == corev1.ConditionTrue {
			return h(live.Degraded, "%s", c.Message)
		}
	}
	if rs.Spec.Replicas != nil && rs.Status.AvailableReplicas < *rs.Spec.Replicas {
		return h(live.Progressing, "Waiting for rollout to finish: %d out of %d new replicas are available...", rs.Status.AvailableReplicas, *rs.Spec.Replicas)
	}
	return h(live.Healthy, "")
}

func statefulSetHealth(s *appsv1.StatefulSet) *live.Health {
	st := s.Status
	if st.ObservedGeneration == 0 || s.Generation > st.ObservedGeneration {
		return h(live.Progressing, "Waiting for statefulset spec update to be observed...")
	}
	if s.Spec.Replicas != nil && st.ReadyReplicas < *s.Spec.Replicas {
		return h(live.Progressing, "Waiting for %d pods to be ready...", *s.Spec.Replicas-st.ReadyReplicas)
	}
	if s.Spec.UpdateStrategy.Type == appsv1.RollingUpdateStatefulSetStrategyType && s.Spec.UpdateStrategy.RollingUpdate != nil &&
		s.Spec.Replicas != nil && s.Spec.UpdateStrategy.RollingUpdate.Partition != nil {
		if want := *s.Spec.Replicas - *s.Spec.UpdateStrategy.RollingUpdate.Partition; st.UpdatedReplicas < want {
			return h(live.Progressing, "Waiting for partitioned roll out to finish: %d out of %d new pods have been updated...", st.UpdatedReplicas, want)
		}
		return h(live.Healthy, "partitioned roll out complete: %d new pods have been updated...", st.UpdatedReplicas)
	}
	if s.Spec.UpdateStrategy.Type == appsv1.OnDeleteStatefulSetStrategyType {
		return h(live.Healthy, "statefulset has %d ready pods", st.ReadyReplicas)
	}
	if st.UpdateRevision != st.CurrentRevision {
		return h(live.Progressing, "waiting for statefulset rolling update to complete %d pods at revision %s...", st.UpdatedReplicas, st.UpdateRevision)
	}
	return h(live.Healthy, "statefulset rolling update complete %d pods at revision %s...", st.CurrentReplicas, st.CurrentRevision)
}

func daemonSetHealth(d *appsv1.DaemonSet) *live.Health {
	st := d.Status
	if d.Generation > st.ObservedGeneration {
		return h(live.Progressing, "Waiting for rollout to finish: observed daemon set generation less than desired generation")
	}
	if d.Spec.UpdateStrategy.Type == appsv1.OnDeleteDaemonSetStrategyType {
		return h(live.Healthy, "daemon set %d out of %d new pods have been updated", st.UpdatedNumberScheduled, st.DesiredNumberScheduled)
	}
	if st.UpdatedNumberScheduled < st.DesiredNumberScheduled {
		return h(live.Progressing, "Waiting for daemon set %q rollout to finish: %d out of %d new pods have been updated...", d.Name, st.UpdatedNumberScheduled, st.DesiredNumberScheduled)
	}
	if st.NumberAvailable < st.DesiredNumberScheduled {
		return h(live.Progressing, "Waiting for daemon set %q rollout to finish: %d of %d updated pods are available...", d.Name, st.NumberAvailable, st.DesiredNumberScheduled)
	}
	return h(live.Healthy, "")
}

// podErrorReasons are container waiting reasons that will not fix themselves.
var podErrorReasons = map[string]bool{
	"CrashLoopBackOff": true, "ImagePullBackOff": true, "ErrImagePull": true, "ErrImageNeverPull": true,
	"InvalidImageName": true, "CreateContainerConfigError": true, "CreateContainerError": true, "RunContainerError": true,
}

func podHealth(p *corev1.Pod) *live.Health {
	if p.DeletionTimestamp != nil {
		return h(live.Progressing, "Pod is terminating")
	}
	for _, list := range [][]corev1.ContainerStatus{p.Status.InitContainerStatuses, p.Status.ContainerStatuses} {
		for _, c := range list {
			if w := c.State.Waiting; w != nil && podErrorReasons[w.Reason] {
				msg := c.Name + ": " + w.Reason
				if w.Message != "" {
					msg += ": " + w.Message
				}
				return &live.Health{Status: live.Degraded, Message: msg}
			}
		}
	}
	switch p.Status.Phase {
	case corev1.PodSucceeded:
		return h(live.Healthy, "%s", p.Status.Message)
	case corev1.PodFailed:
		if p.Status.Message != "" {
			return h(live.Degraded, "%s", p.Status.Message)
		}
		for _, c := range p.Status.ContainerStatuses {
			if t := c.State.Terminated; t != nil && t.ExitCode != 0 {
				return h(live.Degraded, "%s: %s (exit code %d)", c.Name, t.Reason, t.ExitCode)
			}
		}
		return h(live.Degraded, "Pod failed")
	case corev1.PodPending:
		return h(live.Progressing, "%s", p.Status.Message)
	case corev1.PodRunning:
		if p.Spec.RestartPolicy != corev1.RestartPolicyAlways {
			// A pod that runs to completion (a hook, a job's pod) is on its way.
			return h(live.Progressing, "%s", p.Status.Message)
		}
		if podReady(p) {
			return h(live.Healthy, "%s", p.Status.Message)
		}
		for _, c := range p.Status.ContainerStatuses {
			if c.LastTerminationState.Terminated != nil {
				return h(live.Degraded, "%s: restarted after %s", c.Name, strings.ToLower(c.LastTerminationState.Terminated.Reason))
			}
		}
		return h(live.Progressing, "Pod is running but not ready")
	}
	return h(live.Unknown, "%s", p.Status.Message)
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func serviceHealth(s *corev1.Service) *live.Health {
	if s.Spec.Type == corev1.ServiceTypeLoadBalancer && len(s.Status.LoadBalancer.Ingress) == 0 {
		return h(live.Progressing, "Waiting for the load balancer")
	}
	return h(live.Healthy, "")
}

func ingressHealth(i *networkingv1.Ingress) *live.Health {
	if len(i.Status.LoadBalancer.Ingress) == 0 {
		return h(live.Progressing, "Waiting for an address")
	}
	return h(live.Healthy, "")
}

func jobHealth(j *batchv1.Job) *live.Health {
	for _, c := range j.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobFailed:
			return h(live.Degraded, "%s", c.Message)
		case batchv1.JobComplete:
			return h(live.Healthy, "%s", c.Message)
		}
	}
	if j.Spec.Suspend != nil && *j.Spec.Suspend {
		return h(live.Suspended, "Job is suspended")
	}
	return h(live.Progressing, "Job is running")
}

// hpaHealth: Degraded when the autoscaler cannot read its target or its
// metrics, Healthy once it is actively scaling (or able to), otherwise
// Progressing (the first metrics are not in yet).
func hpaHealth(a *autoscalingv2.HorizontalPodAutoscaler) *live.Health {
	var healthy *live.Health
	for _, c := range a.Status.Conditions {
		switch {
		case c.Type == autoscalingv2.AbleToScale && c.Status == corev1.ConditionFalse,
			c.Type == autoscalingv2.ScalingActive && c.Status == corev1.ConditionFalse &&
				c.Reason != "ScalingDisabled" && c.Reason != "":
			return h(live.Degraded, "%s", c.Message)
		case c.Type == autoscalingv2.ScalingActive && c.Status == corev1.ConditionTrue:
			healthy = h(live.Healthy, "%s", c.Message)
		}
	}
	if healthy != nil {
		return healthy
	}
	return h(live.Progressing, "Waiting to autoscale")
}

func pvcHealth(p *corev1.PersistentVolumeClaim) *live.Health {
	switch p.Status.Phase {
	case corev1.ClaimBound:
		return h(live.Healthy, "")
	case corev1.ClaimPending:
		return h(live.Progressing, "Waiting for a volume")
	case corev1.ClaimLost:
		return h(live.Degraded, "The claim lost its volume")
	}
	return h(live.Unknown, "")
}
