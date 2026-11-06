package lb

import (
	"context"
	"net/netip"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// GatePrefix starts every readiness gate of ours.
const GatePrefix = "target-health.lb.ezghcloud.com/"

// GateType is the readiness gate a pod behind service carries until the load balancer finds it
// healthy.
func GateType(service string) string { return GatePrefix + service }

// gates sets each of the Service's unset gates True when the pod's address is healthy in every
// group fed from it, or at once when the Service is gone or no longer a backend, when the pod has
// addresses but no IPv4 one (never a target, D13), or when the target cap left it out: pods never
// stay NotReady behind something that won't health-check them. A pod with no address yet stays
// unset (the pod watch brings it back). It reports whether a targeted pod is still waiting, so
// the caller reads the targets again.
func (r *TargetsReconciler) gates(ctx context.Context, name string, pods []corev1.Pod, backends []Backend, health map[string]map[string]string, capped map[string]bool) (bool, error) {
	gate := corev1.PodConditionType(GateType(name))
	pending := false
	for i := range pods {
		pod := &pods[i]
		if pod.DeletionTimestamp != nil || !hasGate(pod, gate) || conditionStatus(pod, gate) == corev1.ConditionTrue {
			continue
		}
		ip := podIPv4(pod)
		var reason string
		switch {
		case len(backends) == 0:
			reason = "NotLoadBalanced"
		case ip == "" && len(pod.Status.PodIPs) > 0:
			reason = "NoIPv4Address"
		case ip == "":
			continue
		case capped[ip]:
			reason = "TargetLimit"
		default:
			healthy, targeted := true, false
			for _, b := range backends {
				state, ok := health[b.Group][ip]
				healthy, targeted = healthy && state == "healthy", targeted || ok
			}
			if !healthy {
				pending = pending || targeted
				continue
			}
			reason = "TargetHealthy"
		}
		orig := pod.DeepCopy()
		cond := corev1.PodCondition{Type: gate, Status: corev1.ConditionTrue, Reason: reason, LastTransitionTime: metav1.Now()}
		replaced := false
		for j, c := range pod.Status.Conditions {
			if c.Type == gate {
				pod.Status.Conditions[j], replaced = cond, true
			}
		}
		if !replaced {
			pod.Status.Conditions = append(pod.Status.Conditions, cond)
		}
		// Strategic merge on the status subresource: only this condition is sent, so the
		// kubelet's own conditions are never overwritten from a stale copy.
		if err := r.Client.Status().Patch(ctx, pod, client.StrategicMergeFrom(orig)); client.IgnoreNotFound(err) != nil {
			return pending, err
		}
	}
	return pending, nil
}

func hasGate(pod *corev1.Pod, gate corev1.PodConditionType) bool {
	for _, g := range pod.Spec.ReadinessGates {
		if g.ConditionType == gate {
			return true
		}
	}
	return false
}

func conditionStatus(pod *corev1.Pod, t corev1.PodConditionType) corev1.ConditionStatus {
	for _, c := range pod.Status.Conditions {
		if c.Type == t {
			return c.Status
		}
	}
	return corev1.ConditionUnknown
}

func podIPv4(pod *corev1.Pod) string {
	for _, ip := range pod.Status.PodIPs {
		if a, err := netip.ParseAddr(ip.IP); err == nil && a.Is4() {
			return ip.IP
		}
	}
	return ""
}
