package lb

import (
	"context"
	"encoding/json"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// Webhook serving values: every control-plane VM's apiserver calls its own loopback, with the
// serving certificate k8sagent writes from the cluster CA.
const (
	WebhookPath    = "/mutate-pods"
	WebhookPort    = 9446
	WebhookCertDir = "/run/ezgh/webhook"
)

// Webhook adds a readiness gate to each new pod for every Service in its namespace that selects
// it and is a load-balanced backend. It runs on every VM (not leader-elected) from the manager's
// cache. It never refuses a pod: failurePolicy Ignore covers only a failed call, so its own
// errors are logged and the pod admitted unchanged.
type Webhook struct {
	Client   client.Reader
	Backends Backends
}

// Handle answers one pod CREATE.
func (w *Webhook) Handle(ctx context.Context, req admission.Request) admission.Response {
	var pod corev1.Pod
	log := logf.FromContext(ctx)
	if err := json.Unmarshal(req.Object.Raw, &pod); err != nil {
		log.Error(err, "readiness gates: unreadable pod, admitted unchanged")
		return admission.Allowed("")
	}
	var svcs corev1.ServiceList
	if err := w.Client.List(ctx, &svcs, client.InNamespace(req.Namespace)); err != nil {
		log.Error(err, "readiness gates: can't list Services, pod admitted unchanged", "namespace", req.Namespace)
		return admission.Allowed("")
	}
	sort.Slice(svcs.Items, func(i, j int) bool { return svcs.Items[i].Name < svcs.Items[j].Name })
	changed := false
	for i := range svcs.Items {
		svc := &svcs.Items[i]
		gate := corev1.PodConditionType(GateType(svc.Name))
		if len(svc.Spec.Selector) == 0 || !labels.SelectorFromSet(svc.Spec.Selector).Matches(labels.Set(pod.Labels)) || hasGate(&pod, gate) {
			continue
		}
		if b, err := w.Backends.For(ctx, w.Client, svc); err != nil || len(b) == 0 {
			continue
		}
		pod.Spec.ReadinessGates = append(pod.Spec.ReadinessGates, corev1.PodReadinessGate{ConditionType: gate})
		changed = true
	}
	if !changed {
		return admission.Allowed("")
	}
	raw, err := json.Marshal(&pod)
	if err != nil {
		log.Error(err, "readiness gates: pod admitted unchanged")
		return admission.Allowed("")
	}
	return admission.PatchResponseFromRaw(req.Object.Raw, raw)
}
