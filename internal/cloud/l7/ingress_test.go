package l7

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/cloud/config"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/cloud/lb"
)

func svc(ns, name string, ports ...corev1.ServicePort) *corev1.Service {
	return &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: types.UID("svc-" + name)}, Spec: corev1.ServiceSpec{Ports: ports}}
}

func port(name string, n int32) corev1.ServicePort { return corev1.ServicePort{Name: name, Port: n} }

func backend(name string, port int32) networkingv1.IngressBackend {
	return networkingv1.IngressBackend{Service: &networkingv1.IngressServiceBackend{Name: name, Port: networkingv1.ServiceBackendPort{Number: port}}}
}

func path(t networkingv1.PathType, p string, b networkingv1.IngressBackend) networkingv1.HTTPIngressPath {
	return networkingv1.HTTPIngressPath{Path: p, PathType: &t, Backend: b}
}

func ingress(ns, name string, age int, ann map[string]string, rules ...networkingv1.IngressRule) *networkingv1.Ingress {
	return &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: types.UID("uid-" + name), Annotations: ann, CreationTimestamp: metav1.NewTime(time.Unix(int64(1000+age), 0))},
		Spec:       networkingv1.IngressSpec{IngressClassName: ptr.To(ClassName), Rules: rules},
	}
}

func hostRule(host string, paths ...networkingv1.HTTPIngressPath) networkingv1.IngressRule {
	return networkingv1.IngressRule{Host: host, IngressRuleValue: networkingv1.IngressRuleValue{HTTP: &networkingv1.HTTPIngressRuleValue{Paths: paths}}}
}

func services(ss ...*corev1.Service) map[string]*corev1.Service {
	out := map[string]*corev1.Service{}
	for _, s := range ss {
		out[s.Namespace+"/"+s.Name] = s
	}
	return out
}

func describe(rules []Rule) string {
	var out []string
	for _, r := range rules {
		target := r.Action.Type
		if r.Action.Type == "forward" {
			target = r.Action.Forward[0].Backend
		} else if r.Action.Fixed != nil {
			target = fmt.Sprint(r.Action.Fixed.StatusCode)
		}
		out = append(out, fmt.Sprintf("%v%v>%s", r.Hosts, r.Paths, target))
	}
	return strings.Join(out, " ")
}

func TestIngressHostsAndPaths(t *testing.T) {
	ing := ingress("shop", "web", 0, nil, hostRule("Shop.Example.com",
		path(networkingv1.PathTypePrefix, "/", backend("web", 80)),
		path(networkingv1.PathTypeExact, "/healthz", backend("web", 80)),
		path(networkingv1.PathTypeImplementationSpecific, "/api/", backend("api", 8080)),
	))
	ing.Spec.DefaultBackend = ptr.To(backend("web", 80))
	m, refused := FromIngresses("shop/web", []*networkingv1.Ingress{ing}, services(svc("shop", "web", port("http", 80)), svc("shop", "api", port("", 8080))), nil, testCfg)
	if len(refused) != 0 || len(m.Listeners) != 1 || m.Name != "k8s-ing-shop-web" || m.Key != "ing-uid-web" || m.Tags["ezgh:kubernetes-service"] != "shop/web" {
		t.Fatal(m, refused)
	}
	l := m.Listeners[0]
	want := "[shop.example.com][{exact /healthz}]>shop.web.80 [shop.example.com][{prefix /api}]>shop.api.8080 [shop.example.com][{prefix /}]>shop.web.80"
	if got := describe(l.Rules); got != want || l.Port != 80 || l.Default.Forward[0].Backend != "shop.web.80" || len(m.Backends) != 2 {
		t.Fatalf("%s\nwant %s\n%+v", got, want, l.Default)
	}
}

func TestGroupSharesOneLoadBalancer(t *testing.T) {
	g := map[string]string{GroupAnnotation: "shop"}
	newer := ingress("shop", "cart", 5, g, hostRule("", path(networkingv1.PathTypePrefix, "/cart", backend("cart", 80))))
	older := ingress("shop", "home", 1, map[string]string{GroupAnnotation: "shop"}, hostRule("", path(networkingv1.PathTypePrefix, "/cart", backend("home", 80))))
	m, _ := FromIngresses("shop/shop", []*networkingv1.Ingress{newer, older}, services(svc("shop", "home", port("", 80)), svc("shop", "cart", port("", 80))), nil, testCfg)
	// Same precedence: the older Ingress first.
	if got := describe(m.Listeners[0].Rules); got != "[][{prefix /cart}]>shop.home.80 [][{prefix /cart}]>shop.cart.80" || m.Name != "k8s-ing-shop-shop" || m.Key != "ing-uid-home" ||
		m.Tags["ezgh:kubernetes-service"] != "shop/shop" {
		t.Fatal(got, m.Name)
	}
	if g, ok := IngressGroup(newer); !ok || g != "shop/shop" {
		t.Fatal(g)
	}
	newer.Annotations[GroupAnnotation] = "Not_A_Label"
	if _, ok := IngressGroup(newer); ok {
		t.Fatal("invalid group accepted")
	}
}

// A group is one namespace's; two namespaces naming group shop get two load
// balancers, each tagged with its own <ns>/shop.
func TestGroupsStayInTheirNamespace(t *testing.T) {
	a := ingress("a", "home", 1, map[string]string{GroupAnnotation: "shop"}, hostRule("", path(networkingv1.PathTypePrefix, "/", backend("home", 80))))
	b := ingress("b", "cart", 2, map[string]string{GroupAnnotation: "shop"}, hostRule("", path(networkingv1.PathTypePrefix, "/", backend("cart", 80))))
	r, k, _, f := newIngressReconciler(t, a, b, svc("a", "home", port("", 80)), svc("b", "cart", port("", 80)))
	reconcileGroup(t, r, "a/shop", 2)
	reconcileGroup(t, r, "b/shop", 2)
	sa, _ := readState(getIngress(t, k, "a", "home").Annotations)
	sb, _ := readState(getIngress(t, k, "b", "cart").Annotations)
	if sa.LoadBalancer == "" || sb.LoadBalancer == "" || sa.LoadBalancer == sb.LoadBalancer || sa.Group != "a/shop" || sb.Group != "b/shop" || len(sa.Groups) != 1 {
		t.Fatal(sa, sb)
	}
	if f.lbs[sa.LoadBalancer]["name"] != "k8s-ing-a-shop" || f.lbs[sb.LoadBalancer]["tags"].(map[string]any)["ezgh:kubernetes-service"] != "b/shop" {
		t.Fatal(f.lbs)
	}
}

func TestSSLRedirect(t *testing.T) {
	ing := ingress("shop", "web", 0, map[string]string{CertificatesAnnotation: "crt_a, crt_b", SSLRedirectAnnotation: "true"},
		hostRule("shop.example.com", path(networkingv1.PathTypePrefix, "/", backend("web", 80))))
	m, refused := FromIngresses("shop/web", []*networkingv1.Ingress{ing}, services(svc("shop", "web", port("", 80))), nil, testCfg)
	if len(refused) != 0 || len(m.Listeners) != 2 {
		t.Fatal(m, refused)
	}
	http, https := m.Listeners[0], m.Listeners[1]
	if http.Default.Type != "redirect" || *http.Default.Redirect.Scheme != "https" || http.Default.Redirect.StatusCode != 301 || len(http.Rules) != 0 {
		t.Fatal(http)
	}
	if https.Port != 443 || https.Protocol != "https" || fmt.Sprint(https.Certificates) != "[crt_a crt_b]" || len(https.Rules) != 1 {
		t.Fatal(https)
	}
}

func TestTLSSecretImportedOrRefused(t *testing.T) {
	ing := ingress("shop", "web", 0, nil, hostRule("shop.example.com", path(networkingv1.PathTypePrefix, "/", backend("web", 80))))
	ing.Spec.TLS = []networkingv1.IngressTLS{{SecretName: "good"}, {SecretName: "bad"}, {SecretName: "missing"}}
	bad := tlsSecret(t, "shop", "bad", "x")
	bad.Data[corev1.TLSPrivateKeyKey] = tlsSecret(t, "shop", "other", "x").Data[corev1.TLSPrivateKeyKey] // not its key
	secrets := map[string]*corev1.Secret{"shop/good": tlsSecret(t, "shop", "good", "shop.example.com"), "shop/bad": bad}
	m, refused := FromIngresses("shop/web", []*networkingv1.Ingress{ing}, services(svc("shop", "web", port("", 80))), secrets, testCfg)
	r := refused["shop/web"]
	if len(r) != 2 || r[0].Reason != InvalidCertificate || !strings.Contains(r[0].Message, "shop/bad") || !strings.Contains(r[1].Message, "shop/missing") {
		t.Fatal(r)
	}
	for _, f := range r {
		if strings.Contains(f.Message, "PRIVATE") {
			t.Fatal("key material in a refusal")
		}
	}
	if len(m.Listeners) != 2 || fmt.Sprint(m.Listeners[1].Secrets) != "[shop/good]" || len(m.Secrets) != 1 {
		t.Fatal(m.Listeners)
	}
}

// A Secret with no DNS names serves only as the group's one certificate (lb-api takes it only as a
// listener's default); beside another, it's refused.
func TestIngressNamelessSecretOnlyAlone(t *testing.T) {
	ing := ingress("shop", "web", 0, nil, hostRule("shop.example.com", path(networkingv1.PathTypePrefix, "/", backend("web", 80))))
	ing.Spec.TLS = []networkingv1.IngressTLS{{SecretName: "default"}}
	secrets := map[string]*corev1.Secret{"shop/default": tlsSecret(t, "shop", "default", "")}
	m, refused := FromIngresses("shop/web", []*networkingv1.Ingress{ing}, services(svc("shop", "web", port("", 80))), secrets, testCfg)
	if len(refused["shop/web"]) != 0 || len(m.Listeners) != 2 || fmt.Sprint(m.Listeners[1].Secrets) != "[shop/default]" || m.Secrets["shop/default"] == nil {
		t.Fatal(m.Listeners, refused)
	}
	ing.Annotations = map[string]string{CertificatesAnnotation: "crt_aaaaaaaaaaaa"}
	m, refused = FromIngresses("shop/web", []*networkingv1.Ingress{ing}, services(svc("shop", "web", port("", 80))), secrets, testCfg)
	r := refused["shop/web"]
	if len(r) != 1 || r[0].Reason != InvalidCertificate || !strings.Contains(r[0].Message, "shop/default has no DNS names") {
		t.Fatal(r)
	}
	if len(m.Listeners) != 2 || fmt.Sprint(m.Listeners[1].Certificates) != "[crt_aaaaaaaaaaaa]" || len(m.Listeners[1].Secrets) != 0 || len(m.Secrets) != 0 {
		t.Fatal(m.Listeners, len(m.Secrets))
	}
	// Two with no names: the first serves, the other is refused to the Ingress naming it.
	other := ingress("shop", "other", 1, nil)
	other.Spec.TLS = []networkingv1.IngressTLS{{SecretName: "other"}}
	ing.Annotations = nil
	secrets["shop/other"] = tlsSecret(t, "shop", "other", "")
	m, refused = FromIngresses("shop/web", []*networkingv1.Ingress{ing, other}, services(svc("shop", "web", port("", 80))), secrets, testCfg)
	if r := refused["shop/other"]; len(refused["shop/web"]) != 0 || len(r) != 1 || !strings.Contains(r[0].Message, "shop/other has no DNS names") {
		t.Fatal(refused)
	}
	if len(m.Listeners) != 2 || fmt.Sprint(m.Listeners[1].Secrets) != "[shop/default]" || len(m.Secrets) != 1 {
		t.Fatal(m.Listeners, len(m.Secrets))
	}
}

func TestMissingServiceIs503(t *testing.T) {
	ing := ingress("shop", "web", 0, nil, hostRule("", path(networkingv1.PathTypePrefix, "/", backend("gone", 80)), path(networkingv1.PathTypePrefix, "/x", backend("web", 81))))
	m, refused := FromIngresses("shop/web", []*networkingv1.Ingress{ing}, services(svc("shop", "web", port("", 80))), nil, testCfg)
	if got := describe(m.Listeners[0].Rules); got != "[][{prefix /x}]>503 [][{prefix /}]>503" || len(refused["shop/web"]) != 2 || refused["shop/web"][0].Reason != BackendNotFound || len(m.Backends) != 0 {
		t.Fatal(got, refused)
	}
}

func newIngressReconciler(t *testing.T, objs ...client.Object) (*IngressReconciler, client.Client, *events.FakeRecorder, *fakeEZGH) {
	f, c := newFake(t)
	scheme := clientgoscheme.Scheme
	k := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).WithStatusSubresource(&networkingv1.Ingress{}).Build()
	rec := events.NewFakeRecorder(100)
	return &IngressReconciler{Client: k, Clients: c, Config: func() (config.Config, error) { return testCfg, nil }, Recorder: rec}, k, rec, f
}

func getIngress(t *testing.T, k client.Client, ns, name string) *networkingv1.Ingress {
	t.Helper()
	var ing networkingv1.Ingress
	if err := k.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &ing); err != nil {
		return nil
	}
	return &ing
}

func reconcileGroup(t *testing.T, r *IngressReconciler, group string, times int) {
	t.Helper()
	for range times {
		if _, err := r.Reconcile(ctx, groupRequest(group)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLastIngressDeletesLoadBalancer(t *testing.T) {
	a := ingress("shop", "home", 1, map[string]string{GroupAnnotation: "shop"}, hostRule("", path(networkingv1.PathTypePrefix, "/", backend("home", 80))))
	b := ingress("shop", "cart", 2, map[string]string{GroupAnnotation: "shop"}, hostRule("", path(networkingv1.PathTypePrefix, "/cart", backend("cart", 80))))
	r, k, _, f := newIngressReconciler(t, a, b, svc("shop", "home", port("", 80)), svc("shop", "cart", port("", 80)))
	reconcileGroup(t, r, "shop/shop", 2)
	ha, hb := getIngress(t, k, "shop", "home"), getIngress(t, k, "shop", "cart")
	sa, _ := readState(ha.Annotations)
	sb, _ := readState(hb.Annotations)
	if sa.LoadBalancer == "" || sa.LoadBalancer != sb.LoadBalancer || sa.Group != "shop/shop" || len(sa.Groups) != 2 || len(f.listeners) != 1 ||
		len(ha.Finalizers) != 1 || len(hb.Status.LoadBalancer.Ingress) != 2 || hb.Status.LoadBalancer.Ingress[0].IP != "203.0.113.9" {
		t.Fatal(sa, hb.Status)
	}
	// Targets come from the state: both Services feed a group, owned by the group.
	if got, _ := IngressBackends(ctx, k, svc("shop", "cart", port("", 80))); len(got) != 1 || got[0].Group != sa.Groups["shop.cart.80"] || got[0].Owner != "shop/shop" {
		t.Fatal(got)
	}

	// The first goes: the load balancer stays, without its rules, and it's let go at once.
	if err := k.Delete(ctx, ha); err != nil {
		t.Fatal(err)
	}
	reconcileGroup(t, r, "shop/shop", 1)
	if getIngress(t, k, "shop", "home") != nil || len(f.lbs) != 2 || len(f.groups) != 1 {
		t.Fatal("first Ingress held, or load balancer gone", f.groups)
	}
	// The last goes: one reconcile per step, each asking to be read again until all is gone.
	if err := k.Delete(ctx, getIngress(t, k, "shop", "cart")); err != nil {
		t.Fatal(err)
	}
	res, err := r.Reconcile(ctx, groupRequest("shop/shop"))
	if err != nil || res.RequeueAfter == 0 || getIngress(t, k, "shop", "cart") == nil {
		t.Fatal(res, err)
	}
	for _, lbr := range f.lbs {
		lbr["status"] = "deleted"
	}
	if res, err = r.Reconcile(ctx, groupRequest("shop/shop")); err != nil || res.RequeueAfter != 0 {
		t.Fatal(res, err)
	}
	if getIngress(t, k, "shop", "cart") != nil || len(f.groups) != 0 {
		t.Fatal("not torn down", f.log())
	}
}

// Fix: an Ingress moved to another group of its namespace waits until its old group has let go of
// what it made, so nothing leaks.
func TestMoveBetweenGroupsReleasesTheOldOne(t *testing.T) {
	ing := ingress("shop", "web", 1, map[string]string{GroupAnnotation: "old"}, hostRule("", path(networkingv1.PathTypePrefix, "/", backend("web", 80))))
	r, k, _, f := newIngressReconciler(t, ing, svc("shop", "web", port("", 80)))
	reconcileGroup(t, r, "shop/old", 2)
	old, _ := readState(getIngress(t, k, "shop", "web").Annotations)
	moved := getIngress(t, k, "shop", "web")
	moved.Annotations[GroupAnnotation] = "new"
	if err := k.Update(ctx, moved); err != nil {
		t.Fatal(err)
	}
	f.reset()
	// The new group waits: its member still holds the old group's state.
	if res, err := r.Reconcile(ctx, groupRequest("shop/new")); err != nil || res.RequeueAfter == 0 || len(f.log()) != 0 {
		t.Fatal(res, err, f.log())
	}
	// The old group, with no members, tears down from the state on the leaver.
	if res, err := r.Reconcile(ctx, groupRequest("shop/old")); err != nil || res.RequeueAfter == 0 {
		t.Fatal(res, err)
	}
	f.lbs[old.LoadBalancer]["status"] = "deleted"
	if _, err := r.Reconcile(ctx, groupRequest("shop/old")); err != nil {
		t.Fatal(err)
	}
	if st, _ := readState(getIngress(t, k, "shop", "web").Annotations); !st.Empty() || st.Group != "shop/old" || st.Epoch == 0 || len(f.groups) != 0 {
		t.Fatal(st, f.groups) // the epoch stays, so the name's next group never replays a key
	}
	reconcileGroup(t, r, "shop/new", 2)
	st, _ := readState(getIngress(t, k, "shop", "web").Annotations)
	if st.Group != "shop/new" || st.LoadBalancer == "" || f.lbs[st.LoadBalancer]["name"] != "k8s-ing-shop-new" || len(getIngress(t, k, "shop", "web").Finalizers) != 1 {
		t.Fatal(st)
	}
}

// Fix: a state written by hand naming another's certificate never deletes it.
func TestForgedIngressStateDeletesNothing(t *testing.T) {
	ing := ingress("shop", "web", 1, nil, hostRule("", path(networkingv1.PathTypePrefix, "/", backend("web", 80))))
	forged, _ := json.Marshal(State{Group: "shop/web", Certificates: map[string]Imported{"x/y": {ID: "crt_prod"}}})
	ing.Annotations = map[string]string{lb.StateAnnotation: string(forged)}
	r, _, rec, f := newIngressReconciler(t, ing, svc("shop", "web", port("", 80)))
	f.certs["crt_prod"] = map[string]any{"slug": "crt_prod", "name": "prod", "tags": map[string]any{"team": "prod"}}
	reconcileGroup(t, r, "shop/web", 2)
	if f.certs["crt_prod"] == nil {
		t.Fatal("deleted", f.log())
	}
	found := false
	for len(rec.Events) > 0 {
		found = found || strings.Contains(<-rec.Events, "certificate crt_prod")
	}
	if !found {
		t.Fatal("no warning")
	}
}

// Review Focus 1: a refused create lands on every Ingress of the group, requeues, deletes nothing.
func TestIngressDeniedGetsEvent(t *testing.T) {
	a := ingress("shop", "home", 1, map[string]string{GroupAnnotation: "shop"}, hostRule("", path(networkingv1.PathTypePrefix, "/", backend("home", 80))))
	b := ingress("shop", "cart", 2, map[string]string{GroupAnnotation: "shop"}, hostRule("", path(networkingv1.PathTypePrefix, "/cart", backend("cart", 80))))
	r, k, rec, f := newIngressReconciler(t, a, b, svc("shop", "home", port("", 80)), svc("shop", "cart", port("", 80)))
	f.fail["POST /load-balancers"] = [2]string{"403", "access_denied"}
	if _, err := r.Reconcile(ctx, groupRequest("shop/shop")); err == nil {
		t.Fatal("no error to requeue")
	}
	var got []string
	for len(rec.Events) > 0 {
		got = append(got, <-rec.Events)
	}
	if len(got) != 2 || !strings.HasPrefix(got[0], "Warning MissingPermission") || !strings.Contains(got[1], "lb.loadBalancers.create") {
		t.Fatal(got)
	}
	for _, w := range f.log() {
		if strings.HasPrefix(w, "DELETE") {
			t.Fatal(f.log())
		}
	}
	// What was made is recorded, and both still hold the finalizer.
	if st, _ := readState(getIngress(t, k, "shop", "cart").Annotations); len(st.Groups) != 2 || !st.OwnIPv4 || getIngress(t, k, "shop", "home").Finalizers[0] != lb.Finalizer {
		t.Fatal(st)
	}
}
