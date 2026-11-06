package lb

import (
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// An Ingress group or Gateway carrying the same tags as a Service (ezgh:kubernetes-service is
// "<ns>/<name>" for all three) never adopts the Service's load balancer: the name must match too.
func TestAdoptNeedsTheName(t *testing.T) {
	f, c := newFake(t)
	svc := twoPorts()
	converge(t, c, State{}, model(t, svc))
	f.reset()
	other := model(t, svc)
	other.Name = "k8s-gw-shop-web"
	st, err := Apply(ctx, c, testCfg, State{}, other)
	if err != nil || st.LoadBalancer == "" || len(st.Groups) != 2 {
		t.Fatal(err, st)
	}
	hasPrefixes(t, f.log(), "POST /ipv4-addresses", "POST /target-groups", "POST /target-groups", "POST /load-balancers")
	// Tearing down a lost state by tags alone deletes nothing of the Service's.
	f.reset()
	if _, err := Apply(ctx, c, testCfg, State{}, Model{Tags: other.Tags, Named: "k8s-ing-shop-web"}); err != nil || len(f.log()) != 0 {
		t.Fatal(err, f.log())
	}
}

// A state annotation is anyone's to write who can edit the object: one copied from another
// Service names resources that aren't this Service's, which are forgotten, never changed or
// deleted.
func TestForgedStateDeletesNothing(t *testing.T) {
	f, c := newFake(t)
	victim := converge(t, c, State{}, model(t, twoPorts()))
	evil := twoPorts()
	evil.Namespace, evil.Name, evil.UID = "evil", "svc", "u2"
	f.reset()
	got, err := Apply(ctx, c, testCfg, victim, teardownModel(evil))
	if err != nil || !got.Empty() || len(got.Forged) != 4 || len(f.log()) != 0 {
		t.Fatal(err, got, f.log())
	}
	// Live: the forged load balancer is forgotten and the next pass makes its own; the victim's
	// groups and address are never touched.
	got, err = Apply(ctx, c, testCfg, victim, model(t, evil))
	if err != nil || got.LoadBalancer != "" || len(got.Forged) != 1 || !strings.HasPrefix(got.Forged[0], "load balancer ") {
		t.Fatal(err, got)
	}
	for _, w := range f.log() {
		for _, id := range append([]string{victim.LoadBalancer, victim.IPv4Address}, victim.Groups["u1/http"], victim.Groups["u1/admin"]) {
			if strings.Contains(w, id) {
				t.Fatal("touched", id, f.log())
			}
		}
	}
	got = converge(t, c, got, model(t, evil))
	if got.IPv4Address == victim.IPv4Address || got.Groups["u2/http"] == victim.Groups["u1/http"] {
		t.Fatal(got)
	}
}

// The targets controller feeds only groups carrying the cluster's tag and the Service's own.
func TestForgedTargetGroupIgnored(t *testing.T) {
	victim := service([]corev1.ServicePort{tcpPort("http", 80, intstr.FromInt32(8080))}, nil)
	f, c := newFake(t)
	vst := converge(t, c, State{}, model(t, victim))
	// The victim's state copied onto another Service with the same UID-keyed groups.
	other := service([]corev1.ServicePort{tcpPort("http", 80, intstr.FromInt32(8080))}, nil)
	other.Name = "copy"
	data, _ := json.Marshal(vst)
	other.Annotations = map[string]string{StateAnnotation: string(data)}
	s := slice("a", discoveryv1.AddressTypeIPv4, "http", 8080, ep{ip: "10.0.0.9", pod: "a", ready: true})
	s.Labels = map[string]string{discoveryv1.LabelServiceName: "copy"}
	r, rec := reconciler(t, c, other, &s)
	tr := &TargetsReconciler{Client: r.Client, Clients: c, Config: r.Config, Recorder: rec, Backends: Backends{ServiceBackends}}
	f.reset()
	if _, err := tr.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "shop", Name: "copy"}}); err != nil {
		t.Fatal(err)
	}
	if ev := drain(rec); len(f.log()) != 0 || len(ev) != 1 || !strings.Contains(ev[0], "InvalidAnnotation") {
		t.Fatal(f.log(), ev)
	}
}
