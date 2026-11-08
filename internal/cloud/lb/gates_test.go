package lb

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func admit(t *testing.T, w *Webhook, p *corev1.Pod) admission.Response {
	t.Helper()
	raw, _ := json.Marshal(p)
	return w.Handle(ctx, admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{Namespace: "shop", Operation: admissionv1.Create, Object: runtime.RawExtension{Raw: raw}}})
}

func TestWebhookAddsGate(t *testing.T) {
	// web is load-balanced and selects app=web; plain is a ClusterIP Service selecting it too.
	plain := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "plain"}, Spec: corev1.ServiceSpec{Selector: map[string]string{"app": "web"}}}
	_, r, _, _ := targetsSetup(t, service([]corev1.ServicePort{tcpPort("http", 80, intstr.FromInt32(8080))}, nil), plain)
	w := &Webhook{Client: r.Client, Backends: r.Backends}

	resp := admit(t, w, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "a", Labels: map[string]string{"app": "web"}}})
	patch, _ := json.Marshal(resp.Patches)
	if !resp.Allowed || !strings.Contains(string(patch), `"/spec/readinessGates"`) || !strings.Contains(string(patch), "target-health.lb.ezghcloud.com/web") ||
		strings.Contains(string(patch), "/plain") {
		t.Fatalf("%+v %s", resp, patch)
	}
	resp = admit(t, w, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "b", Labels: map[string]string{"app": "db"}}})
	if !resp.Allowed || len(resp.Patches) != 0 {
		t.Fatalf("%+v", resp)
	}
}

func gateStatus(t *testing.T, r *TargetsReconciler, name string) corev1.ConditionStatus {
	t.Helper()
	var p corev1.Pod
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: "shop", Name: name}, &p); err != nil {
		t.Fatal(err)
	}
	return conditionStatus(&p, corev1.PodConditionType(GateType("web")))
}

func TestGateTrueWhenHealthyEverywhere(t *testing.T) {
	p := pod("a", "10.0.0.1", true, map[string]corev1.ConditionStatus{GateType("web"): ""})
	http := slice("h", discoveryv1.AddressTypeIPv4, "http", 8080, ep{ip: "10.0.0.1", pod: "a"})
	admin := slice("m", discoveryv1.AddressTypeIPv4, "admin", 9000, ep{ip: "10.0.0.1", pod: "a"})
	f, r, _, st := targetsSetup(t, twoPorts(), p, &http, &admin)
	res, err := r.Reconcile(ctx, webKey)
	if err != nil || res.RequeueAfter != gatePoll || gateStatus(t, r, "a") != corev1.ConditionUnknown {
		t.Fatal(res, err)
	}
	healthy := func(group string) {
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, tg := range f.targets {
			if tg.TargetGroup == group {
				tg.State = "healthy"
			}
		}
	}
	healthy(st.Groups["u1/http"])
	if res, err = r.Reconcile(ctx, webKey); err != nil || res.RequeueAfter != gatePoll || gateStatus(t, r, "a") != corev1.ConditionUnknown {
		t.Fatal("healthy in one of two groups must stay unset", res, err)
	}
	healthy(st.Groups["u1/admin"])
	if res, err = r.Reconcile(ctx, webKey); err != nil || res.RequeueAfter != 0 || gateStatus(t, r, "a") != corev1.ConditionTrue {
		t.Fatal(res, err)
	}
	// The kubelet's own conditions are untouched.
	var got corev1.Pod
	_ = r.Client.Get(ctx, types.NamespacedName{Namespace: "shop", Name: "a"}, &got)
	if conditionStatus(&got, corev1.ContainersReady) != corev1.ConditionTrue {
		t.Fatalf("%+v", got.Status.Conditions)
	}
	// Nothing pending: no targets read on the next run beyond the sync.
	if res, _ = r.Reconcile(ctx, webKey); res.RequeueAfter != 0 {
		t.Fatal(res)
	}
}

func TestOrphanGateReleased(t *testing.T) {
	p := pod("a", "10.0.0.1", true, map[string]corev1.ConditionStatus{GateType("web"): ""})
	s := slice("h", discoveryv1.AddressTypeIPv4, "http", 8080, ep{ip: "10.0.0.1", pod: "a"})
	f, r, _, _ := targetsSetup(t, service([]corev1.ServicePort{tcpPort("http", 80, intstr.FromInt32(8080))}, nil), p, &s)
	if res, _ := r.Reconcile(ctx, webKey); res.RequeueAfter != gatePoll {
		t.Fatal(res)
	}
	var svc corev1.Service
	_ = r.Client.Get(ctx, webKey.NamespacedName, &svc)
	svc.Spec.Type = corev1.ServiceTypeClusterIP
	if err := r.Client.Update(ctx, &svc); err != nil {
		t.Fatal(err)
	}
	f.reset()
	if res, err := r.Reconcile(ctx, webKey); err != nil || res.RequeueAfter != 0 || gateStatus(t, r, "a") != corev1.ConditionTrue || len(f.log()) != 0 {
		t.Fatal(res, err, f.log())
	}
}

func TestGateReleasedWhenServiceGone(t *testing.T) {
	p := pod("a", "10.0.0.1", true, map[string]corev1.ConditionStatus{GateType("web"): ""})
	_, r, _, _ := targetsSetup(t, service([]corev1.ServicePort{tcpPort("http", 80, intstr.FromInt32(8080))}, nil), p)
	var svc corev1.Service
	_ = r.Client.Get(ctx, webKey.NamespacedName, &svc)
	if err := r.Client.Delete(ctx, &svc); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, webKey); err != nil || gateStatus(t, r, "a") != corev1.ConditionTrue {
		t.Fatal(err)
	}
}

func TestPodGateServices(t *testing.T) {
	p := pod("a", "10.0.0.1", true, map[string]corev1.ConditionStatus{GateType("web"): "", GateType("api"): corev1.ConditionTrue, "example.com/x": ""})
	p.Namespace = "shop"
	got := PodGateServices(ctx, p)
	if len(got) != 1 || got[0].NamespacedName != webKey.NamespacedName {
		t.Fatal(got)
	}
}

func TestWebhookNeverRefuses(t *testing.T) {
	_, r, _, _ := targetsSetup(t, service([]corev1.ServicePort{tcpPort("http", 80, intstr.FromInt32(8080))}, nil))
	w := &Webhook{Client: r.Client, Backends: r.Backends}
	resp := w.Handle(ctx, admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{Namespace: "shop", Object: runtime.RawExtension{Raw: []byte("{not json")}}})
	if !resp.Allowed || len(resp.Patches) != 0 {
		t.Fatalf("%+v", resp)
	}
}

func TestGateWaitsForAnAddress(t *testing.T) {
	fresh := pod("fresh", "", true, map[string]corev1.ConditionStatus{GateType("web"): ""})
	fresh.Status.PodIPs, fresh.Status.PodIP = nil, ""
	v6 := pod("v6", "fd00::7", true, map[string]corev1.ConditionStatus{GateType("web"): ""})
	_, r, _, _ := targetsSetup(t, service([]corev1.ServicePort{tcpPort("http", 80, intstr.FromInt32(8080))}, nil), fresh, v6)
	res, err := r.Reconcile(ctx, webKey)
	// No address yet: held, and not polled (the pod watch brings it back). IPv6 only: never a target, so not held.
	if err != nil || res.RequeueAfter != 0 || gateStatus(t, r, "fresh") != corev1.ConditionUnknown || gateStatus(t, r, "v6") != corev1.ConditionTrue {
		t.Fatal(res, err)
	}
}

func TestCappedPodsReleasedNotPolled(t *testing.T) {
	var eps []ep
	objs := []client.Object{}
	for i := range MaxTargets + 1 {
		name, ip := fmt.Sprint("p", i), fmt.Sprintf("10.0.%d.%d", i/250, i%250+1)
		eps = append(eps, ep{ip: ip, pod: name, ready: true})
		objs = append(objs, pod(name, ip, true, map[string]corev1.ConditionStatus{GateType("web"): ""}))
	}
	s := slice("a", discoveryv1.AddressTypeIPv4, "http", 8080, eps...)
	f, r, _, st := targetsSetup(t, service([]corev1.ServicePort{tcpPort("http", 80, intstr.FromInt32(8080))}, nil), append(objs, &s)...)
	if res, err := r.Reconcile(ctx, webKey); err != nil || res.RequeueAfter != gatePoll {
		t.Fatal(res, err)
	}
	last := fmt.Sprint("p", MaxTargets) // 10.0.2.1 sorts last
	var p corev1.Pod
	_ = r.Client.Get(ctx, types.NamespacedName{Namespace: "shop", Name: last}, &p)
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodConditionType(GateType("web")) && (c.Status != corev1.ConditionTrue || c.Reason != "TargetLimit") {
			t.Fatalf("%+v", c)
		}
	}
	if gateStatus(t, r, last) != corev1.ConditionTrue || gateStatus(t, r, "p0") != corev1.ConditionUnknown {
		t.Fatal("capped pod held, or targeted pod released")
	}
	// All targets healthy: nothing left to poll for.
	f.mu.Lock()
	for _, tg := range f.targets {
		if tg.TargetGroup == st.Groups["u1/http"] {
			tg.State = "healthy"
		}
	}
	f.mu.Unlock()
	if res, err := r.Reconcile(ctx, webKey); err != nil || res.RequeueAfter != 0 {
		t.Fatal(res, err)
	}
}

func TestTargetLimitConflictIsAnEvent(t *testing.T) {
	s := slice("a", discoveryv1.AddressTypeIPv4, "http", 8080, ep{ip: "10.0.0.1", pod: "a", ready: true})
	f, r, rec, st := targetsSetup(t, service([]corev1.ServicePort{tcpPort("http", 80, intstr.FromInt32(8080))}, nil), &s)
	f.fail["POST /target-groups/"+st.Groups["u1/http"]+"/targets"] = [2]string{"409", "target_limit"}
	res, err := r.Reconcile(ctx, webKey)
	if err != nil || res.RequeueAfter != drainWait {
		t.Fatal(res, err)
	}
	if ev := drain(rec); len(ev) != 1 || !strings.HasPrefix(ev[0], "Warning TargetLimit ") {
		t.Fatal(ev)
	}
}

func TestGateReleasedWhenCreateFailed(t *testing.T) {
	p := pod("a", "10.0.0.1", true, map[string]corev1.ConditionStatus{GateType("web"): ""})
	s := slice("h", discoveryv1.AddressTypeIPv4, "http", 8080, ep{ip: "10.0.0.1", pod: "a"})
	_, r, _, st := targetsSetup(t, service([]corev1.ServicePort{tcpPort("http", 80, intstr.FromInt32(8080))}, nil), p, &s)
	setState := func(mut State) {
		var svc corev1.Service
		_ = r.Client.Get(ctx, webKey.NamespacedName, &svc)
		data, _ := json.Marshal(mut)
		svc.Annotations[StateAnnotation] = string(data)
		if err := r.Client.Update(ctx, &svc); err != nil {
			t.Fatal(err)
		}
	}
	// Review Focus 1: a load balancer still being created keeps gating.
	setState(State{Groups: st.Groups, LoadBalancer: "lbr_a", Status: "provisioning"})
	if res, err := r.Reconcile(ctx, webKey); err != nil || res.RequeueAfter == 0 || gateStatus(t, r, "a") == corev1.ConditionTrue {
		t.Fatal(res, err)
	}
	// A failed create recorded no load balancer: nothing to wait for.
	setState(State{Groups: st.Groups, Failed: "fp"})
	res, err := r.Reconcile(ctx, webKey)
	if err != nil || res.RequeueAfter != 0 || gateStatus(t, r, "a") != corev1.ConditionTrue { // reason NotLoadBalanced
		t.Fatal(res, err)
	}
	newPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "b", Labels: map[string]string{"app": "web"}}}
	if resp := admit(t, &Webhook{Client: r.Client, Backends: r.Backends}, newPod); len(resp.Patches) != 0 {
		t.Fatal(resp)
	}
}
