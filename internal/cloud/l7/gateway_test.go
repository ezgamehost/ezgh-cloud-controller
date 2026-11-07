package l7

import (
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/cloud/config"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/cloud/lb"
)

func listener(name string, proto gatewayv1.ProtocolType, port int, host string) gatewayv1.Listener {
	l := gatewayv1.Listener{Name: gatewayv1.SectionName(name), Protocol: proto, Port: gatewayv1.PortNumber(port)}
	if host != "" {
		l.Hostname = ptr.To(gatewayv1.Hostname(host))
	}
	return l
}

func gateway(listeners ...gatewayv1.Listener) *gatewayv1.Gateway {
	return &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: "infra", Name: "edge", UID: "gw-uid", Generation: 1},
		Spec:       gatewayv1.GatewaySpec{GatewayClassName: ClassName, Listeners: listeners},
	}
}

func allNamespaces(l gatewayv1.Listener) gatewayv1.Listener {
	l.AllowedRoutes = &gatewayv1.AllowedRoutes{Namespaces: &gatewayv1.RouteNamespaces{From: ptr.To(gatewayv1.NamespacesFromAll)}}
	return l
}

func backendRef(ns, name string, port int, weight ...int32) gatewayv1.HTTPBackendRef {
	b := gatewayv1.HTTPBackendRef{BackendRef: gatewayv1.BackendRef{BackendObjectReference: gatewayv1.BackendObjectReference{Name: gatewayv1.ObjectName(name), Port: ptr.To(gatewayv1.PortNumber(port))}}}
	if ns != "" {
		b.Namespace = ptr.To(gatewayv1.Namespace(ns))
	}
	if len(weight) > 0 {
		b.Weight = &weight[0]
	}
	return b
}

func prefixMatch(p string) gatewayv1.HTTPRouteMatch {
	return gatewayv1.HTTPRouteMatch{Path: &gatewayv1.HTTPPathMatch{Type: ptr.To(gatewayv1.PathMatchPathPrefix), Value: ptr.To(p)}}
}

func route(ns, name string, age int, hosts []string, rules ...gatewayv1.HTTPRouteRule) gatewayv1.HTTPRoute {
	r := gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: types.UID("rt-" + name), Generation: 1, CreationTimestamp: metav1.NewTime(time.Unix(int64(1000+age), 0))}}
	r.Spec.ParentRefs = []gatewayv1.ParentReference{{Name: "edge", Namespace: ptr.To(gatewayv1.Namespace("infra"))}}
	for _, h := range hosts {
		r.Spec.Hostnames = append(r.Spec.Hostnames, gatewayv1.Hostname(h))
	}
	r.Spec.Rules = rules
	return r
}

func rule(matches []gatewayv1.HTTPRouteMatch, refs ...gatewayv1.HTTPBackendRef) gatewayv1.HTTPRouteRule {
	return gatewayv1.HTTPRouteRule{Matches: matches, BackendRefs: refs}
}

func input(routes ...gatewayv1.HTTPRoute) GatewayInput {
	return GatewayInput{Routes: routes, Services: services(svc("shop", "web", port("http", 80)), svc("shop", "api", port("", 8080)), svc("infra", "web", port("", 80)))}
}

func parentCond(t *testing.T, st Status, ns, name, typ string) *metav1.Condition {
	t.Helper()
	ps := st.Routes[types.NamespacedName{Namespace: ns, Name: name}]
	if len(ps) == 0 {
		t.Fatalf("no parent status for %s/%s", ns, name)
	}
	return meta.FindStatusCondition(ps[0].Conditions, typ)
}

func TestGatewayOneLoadBalancer(t *testing.T) {
	https := allNamespaces(listener("https", gatewayv1.HTTPSProtocolType, 443, ""))
	https.TLS = &gatewayv1.ListenerTLSConfig{Options: map[gatewayv1.AnnotationKey]gatewayv1.AnnotationValue{CertificatesOption: "crt_a, crt_b"}}
	gw := gateway(allNamespaces(listener("http", gatewayv1.HTTPProtocolType, 80, "")), https)
	m, st := FromGateway(gw, input(
		route("shop", "web", 1, nil, rule([]gatewayv1.HTTPRouteMatch{prefixMatch("/")}, backendRef("", "web", 80))),
		route("shop", "api", 2, nil, rule([]gatewayv1.HTTPRouteMatch{prefixMatch("/api")}, backendRef("", "api", 8080))),
	), testCfg)
	if m.Name != "k8s-gw-infra-edge" || m.Key != "gw-gw-uid" || m.Tags["ezgh:kubernetes-service"] != "infra/edge" || len(m.Listeners) != 2 || len(m.Backends) != 2 {
		t.Fatal(m)
	}
	if got := describe(m.Listeners[1].Rules); got != "[][{prefix /api}]>shop.api.8080 [][{prefix /}]>shop.web.80" || m.Listeners[1].Protocol != "https" || fmt.Sprint(m.Listeners[1].Certificates) != "[crt_a crt_b]" {
		t.Fatal(got, m.Listeners[1])
	}
	if m.Listeners[0].Default.Fixed.StatusCode != 404 || st.Listeners[0].AttachedRoutes != 2 || parentCond(t, st, "shop", "web", "Accepted").Status != metav1.ConditionTrue {
		t.Fatal(st)
	}
	// Routes from another namespace need allowedRoutes: the default is Same.
	_, st = FromGateway(gateway(listener("http", gatewayv1.HTTPProtocolType, 80, "")), input(route("shop", "web", 1, nil, rule(nil, backendRef("", "web", 80)))), testCfg)
	if c := parentCond(t, st, "shop", "web", "Accepted"); c.Status != metav1.ConditionFalse || c.Reason != "NotAllowedByListeners" {
		t.Fatal(c)
	}
}

// A rule with three matches is three lb rules, each in its own precedence position.
func TestOneMatchPerRule(t *testing.T) {
	gw := gateway(allNamespaces(listener("http", gatewayv1.HTTPProtocolType, 80, "")))
	exact := gatewayv1.HTTPRouteMatch{Path: &gatewayv1.HTTPPathMatch{Type: ptr.To(gatewayv1.PathMatchExact), Value: ptr.To("/exact")}}
	m, _ := FromGateway(gw, input(
		route("shop", "a", 1, nil, rule([]gatewayv1.HTTPRouteMatch{prefixMatch("/"), exact, prefixMatch("/long/prefix")}, backendRef("", "web", 80))),
		route("shop", "b", 2, nil, rule([]gatewayv1.HTTPRouteMatch{prefixMatch("/long")}, backendRef("", "api", 8080))),
	), testCfg)
	want := "[][{exact /exact}]>shop.web.80 [][{prefix /long/prefix}]>shop.web.80 [][{prefix /long}]>shop.api.8080 [][{prefix /}]>shop.web.80"
	if got := describe(m.Listeners[0].Rules); got != want {
		t.Fatalf("%s\nwant %s", got, want)
	}
}

func TestListenerHostnameIntersection(t *testing.T) {
	gw := gateway(allNamespaces(listener("wild", gatewayv1.HTTPProtocolType, 80, "*.example.com")), allNamespaces(listener("foo", gatewayv1.HTTPProtocolType, 80, "foo.example.com")))
	m, st := FromGateway(gw, input(
		route("shop", "named", 1, []string{"a.example.com", "other.org", "*.bar.example.com"}, rule(nil, backendRef("", "web", 80))),
		route("shop", "any", 2, nil, rule(nil, backendRef("", "api", 8080))),
		route("shop", "wide", 3, []string{"*.example.com"}, rule(nil, backendRef("", "web", 80))),
		route("shop", "elsewhere", 4, []string{"other.org"}, rule(nil, backendRef("", "web", 80))),
	), testCfg)
	got := describe(m.Listeners[0].Rules)
	// Each listener's hostname ends in its own 404, as the listeners share port 80.
	want := "[a.example.com][{prefix /}]>shop.web.80 [foo.example.com][{prefix /}]>shop.api.8080 [foo.example.com][{prefix /}]>shop.web.80 [foo.example.com][]>404 " +
		"[*.bar.example.com][{prefix /}]>shop.web.80 [*.example.com][{prefix /}]>shop.api.8080 [*.example.com][{prefix /}]>shop.web.80 [*.example.com][]>404"
	if got != want || len(m.Listeners) != 1 {
		t.Fatalf("%s\nwant %s", got, want)
	}
	if c := parentCond(t, st, "shop", "elsewhere", "Accepted"); c.Status != metav1.ConditionFalse || c.Reason != "NoMatchingListenerHostname" {
		t.Fatal(c)
	}
	if st.Listeners[0].AttachedRoutes != 3 || st.Listeners[1].AttachedRoutes != 2 {
		t.Fatal(st.Listeners)
	}
}

func TestHTTPRouteFilters(t *testing.T) {
	gw := gateway(allNamespaces(listener("http", gatewayv1.HTTPProtocolType, 80, "")))
	mod := gatewayv1.HTTPRouteFilter{Type: gatewayv1.HTTPRouteFilterRequestHeaderModifier, RequestHeaderModifier: &gatewayv1.HTTPHeaderFilter{
		Set: []gatewayv1.HTTPHeader{{Name: "X-Env", Value: "prod"}}, Add: []gatewayv1.HTTPHeader{{Name: "X-Via", Value: "ezgh"}}, Remove: []string{"X-Debug"}}}
	resp := gatewayv1.HTTPRouteFilter{Type: gatewayv1.HTTPRouteFilterResponseHeaderModifier, ResponseHeaderModifier: &gatewayv1.HTTPHeaderFilter{Remove: []string{"Server"}}}
	fwd := rule([]gatewayv1.HTTPRouteMatch{prefixMatch("/app")}, backendRef("", "web", 80, 90), backendRef("", "api", 8080, 10))
	fwd.Filters = []gatewayv1.HTTPRouteFilter{mod, resp}
	redirect := rule([]gatewayv1.HTTPRouteMatch{prefixMatch("/old")})
	redirect.Filters = []gatewayv1.HTTPRouteFilter{{Type: gatewayv1.HTTPRouteFilterRequestRedirect, RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{
		Scheme: ptr.To("https"), Hostname: ptr.To(gatewayv1.PreciseHostname("new.example.com")), Port: ptr.To(gatewayv1.PortNumber(8443)),
		Path: &gatewayv1.HTTPPathModifier{Type: gatewayv1.FullPathHTTPPathModifier, ReplaceFullPath: ptr.To("/new")}, StatusCode: ptr.To(301)}}}
	m, _ := FromGateway(gw, input(route("shop", "web", 1, nil, fwd, redirect)), testCfg)
	rules := m.Listeners[0].Rules
	a := rules[0].Action
	if a.Type != "forward" || fmt.Sprint(a.Forward) != "[{shop.web.80 90} {shop.api.8080 10}]" ||
		fmt.Sprint(*a.RequestHeaders) != "{[{X-Env prod}] [{X-Via ezgh}] [X-Debug]}" || fmt.Sprint(a.ResponseHeaders.Remove) != "[Server]" {
		t.Fatalf("%+v", a)
	}
	r := rules[1].Action.Redirect
	if rules[1].Action.Type != "redirect" || *r.Scheme != "https" || *r.Host != "new.example.com" || *r.Port != 8443 || *r.Path != "/new" || r.StatusCode != 301 {
		t.Fatalf("%+v", r)
	}
	// Defaults: a redirect is 302, a backendRef weighs 1.
	redirect.Filters[0].RequestRedirect = &gatewayv1.HTTPRequestRedirectFilter{Scheme: ptr.To("https")}
	m, _ = FromGateway(gw, input(route("shop", "web", 1, nil, redirect, rule(nil, backendRef("", "web", 80)))), testCfg)
	if m.Listeners[0].Rules[0].Action.Redirect.StatusCode != 302 || m.Listeners[0].Rules[1].Action.Forward[0].Weight != 1 {
		t.Fatal(m.Listeners[0].Rules)
	}
}

func TestUnsupportedFilterRejectsRule(t *testing.T) {
	gw := gateway(allNamespaces(listener("http", gatewayv1.HTTPProtocolType, 80, "")))
	rewrite := rule([]gatewayv1.HTTPRouteMatch{prefixMatch("/rw")}, backendRef("", "web", 80))
	rewrite.Filters = []gatewayv1.HTTPRouteFilter{{Type: gatewayv1.HTTPRouteFilterURLRewrite, URLRewrite: &gatewayv1.HTTPURLRewriteFilter{}}}
	regex := rule([]gatewayv1.HTTPRouteMatch{{Path: &gatewayv1.HTTPPathMatch{Type: ptr.To(gatewayv1.PathMatchRegularExpression), Value: ptr.To("/r.*")}}}, backendRef("", "web", 80))
	prefixRedirect := rule(nil)
	prefixRedirect.Filters = []gatewayv1.HTTPRouteFilter{{Type: gatewayv1.HTTPRouteFilterRequestRedirect, RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{
		Path: &gatewayv1.HTTPPathModifier{Type: gatewayv1.PrefixMatchHTTPPathModifier, ReplacePrefixMatch: ptr.To("/x")}}}}
	ok := rule([]gatewayv1.HTTPRouteMatch{prefixMatch("/ok")}, backendRef("", "web", 80))
	m, st := FromGateway(gw, input(route("shop", "mixed", 1, nil, rewrite, regex, prefixRedirect, ok), route("shop", "bad", 2, nil, rewrite)), testCfg)
	if got := describe(m.Listeners[0].Rules); got != "[][{prefix /ok}]>shop.web.80" {
		t.Fatal(got)
	}
	if c := parentCond(t, st, "shop", "mixed", "PartiallyInvalid"); c == nil || c.Reason != "UnsupportedValue" || !strings.Contains(c.Message, "URLRewrite") || !strings.Contains(c.Message, "regular expression") {
		t.Fatal(c)
	}
	if c := parentCond(t, st, "shop", "mixed", "Accepted"); c.Status != metav1.ConditionTrue {
		t.Fatal(c)
	}
	if c := parentCond(t, st, "shop", "bad", "Accepted"); c.Status != metav1.ConditionFalse || c.Reason != "UnsupportedValue" {
		t.Fatal(c)
	}
	// A route that isn't accepted isn't counted.
	if st.Listeners[0].AttachedRoutes != 1 {
		t.Fatal(st.Listeners[0].AttachedRoutes)
	}
}

func TestCrossNamespaceNeedsReferenceGrant(t *testing.T) {
	gw := gateway(allNamespaces(listener("http", gatewayv1.HTTPProtocolType, 80, "")))
	rt := route("shop", "web", 1, nil, rule(nil, backendRef("infra", "web", 80)))
	m, st := FromGateway(gw, input(rt), testCfg)
	if a := m.Listeners[0].Rules[0].Action; a.Type != "fixedResponse" || a.Fixed.StatusCode != 500 || len(m.Backends) != 0 {
		t.Fatal(a)
	}
	if c := parentCond(t, st, "shop", "web", "ResolvedRefs"); c.Status != metav1.ConditionFalse || c.Reason != "RefNotPermitted" {
		t.Fatal(c)
	}
	in := input(rt)
	in.Grants = []gatewayv1.ReferenceGrant{{ObjectMeta: metav1.ObjectMeta{Namespace: "infra", Name: "allow"}, Spec: gatewayv1.ReferenceGrantSpec{
		From: []gatewayv1.ReferenceGrantFrom{{Group: GatewayGroup, Kind: "HTTPRoute", Namespace: "shop"}}, To: []gatewayv1.ReferenceGrantTo{{Group: "", Kind: "Service"}}}}}
	m, st = FromGateway(gw, in, testCfg)
	if a := m.Listeners[0].Rules[0].Action; a.Type != "forward" || a.Forward[0].Backend != "infra.web.80" || parentCond(t, st, "shop", "web", "ResolvedRefs").Status != metav1.ConditionTrue {
		t.Fatal(a)
	}
	// A missing Service and a kind that isn't a Service are 500s too.
	m, st = FromGateway(gw, input(route("shop", "web", 1, nil, rule(nil, backendRef("", "gone", 80)))), testCfg)
	if m.Listeners[0].Rules[0].Action.Fixed.StatusCode != 500 || parentCond(t, st, "shop", "web", "ResolvedRefs").Reason != "BackendNotFound" {
		t.Fatal(st)
	}
}

func TestSecretCertificateRefs(t *testing.T) {
	ref := func(ns, name string) gatewayv1.SecretObjectReference {
		r := gatewayv1.SecretObjectReference{Name: gatewayv1.ObjectName(name)}
		if ns != "" {
			r.Namespace = ptr.To(gatewayv1.Namespace(ns))
		}
		return r
	}
	good := allNamespaces(listener("good", gatewayv1.HTTPSProtocolType, 443, "good.example.com"))
	good.TLS = &gatewayv1.ListenerTLSConfig{CertificateRefs: []gatewayv1.SecretObjectReference{ref("", "good")}}
	bad := allNamespaces(listener("bad", gatewayv1.HTTPSProtocolType, 8443, ""))
	bad.TLS = &gatewayv1.ListenerTLSConfig{CertificateRefs: []gatewayv1.SecretObjectReference{ref("", "opaque")}}
	other := allNamespaces(listener("other", gatewayv1.HTTPSProtocolType, 9443, ""))
	other.TLS = &gatewayv1.ListenerTLSConfig{CertificateRefs: []gatewayv1.SecretObjectReference{ref("shop", "tls")}}
	in := input()
	opaque := tlsSecret(t, "infra", "opaque", "x")
	opaque.Type = corev1.SecretTypeOpaque
	in.Secrets = map[string]*corev1.Secret{"infra/good": tlsSecret(t, "infra", "good", "good.example.com"), "infra/opaque": opaque, "shop/tls": tlsSecret(t, "shop", "tls", "x")}
	m, st := FromGateway(gateway(good, bad, other), in, testCfg)
	if len(m.Listeners) != 1 || fmt.Sprint(m.Listeners[0].Secrets) != "[infra/good]" || len(m.Secrets) != 1 || m.Secrets["infra/good"] == nil {
		t.Fatal(m.Listeners, m.Secrets)
	}
	if c := meta.FindStatusCondition(st.Listeners[1].Conditions, "ResolvedRefs"); c.Reason != "InvalidCertificateRef" || st.Valid["bad"] {
		t.Fatal(c)
	}
	if c := meta.FindStatusCondition(st.Listeners[2].Conditions, "ResolvedRefs"); c.Reason != "RefNotPermitted" {
		t.Fatal(c)
	}
}

// Only a grant in the Secret's namespace, from Gateways in the Gateway's, to that Secret (or all
// Secrets) permits a certificateRef there.
func TestSecretReferenceGrants(t *testing.T) {
	l := allNamespaces(listener("https", gatewayv1.HTTPSProtocolType, 443, ""))
	l.TLS = &gatewayv1.ListenerTLSConfig{CertificateRefs: []gatewayv1.SecretObjectReference{{Name: "certificate", Namespace: ptr.To(gatewayv1.Namespace("web"))}}}
	grant := func(ns, group, kind, fromNS string, name *gatewayv1.ObjectName) gatewayv1.ReferenceGrant {
		return gatewayv1.ReferenceGrant{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "g"}, Spec: gatewayv1.ReferenceGrantSpec{
			From: []gatewayv1.ReferenceGrantFrom{{Group: gatewayv1.Group(group), Kind: gatewayv1.Kind(kind), Namespace: gatewayv1.Namespace(fromNS)}},
			To:   []gatewayv1.ReferenceGrantTo{{Kind: "Secret", Name: name}},
		}}
	}
	for want, g := range map[string]gatewayv1.ReferenceGrant{
		"wrong namespace":  grant("app", GatewayGroup, "Gateway", "infra", nil),
		"wrong from group": grant("web", "example.com", "Gateway", "infra", nil),
		"wrong from kind":  grant("web", GatewayGroup, "HTTPRoute", "infra", nil),
		"wrong from ns":    grant("web", GatewayGroup, "Gateway", "other", nil),
		"another Secret":   grant("web", GatewayGroup, "Gateway", "infra", ptr.To(gatewayv1.ObjectName("other"))),
		"all Secrets":      grant("web", GatewayGroup, "Gateway", "infra", nil),
		"this Secret":      grant("web", GatewayGroup, "Gateway", "infra", ptr.To(gatewayv1.ObjectName("certificate"))),
	} {
		in := input()
		in.Grants = []gatewayv1.ReferenceGrant{g}
		in.Secrets = map[string]*corev1.Secret{"web/certificate": tlsSecret(t, "web", "certificate", "x")}
		_, st := FromGateway(gateway(l), in, testCfg)
		c := meta.FindStatusCondition(st.Listeners[0].Conditions, "ResolvedRefs")
		if permitted := want == "all Secrets" || want == "this Secret"; permitted != (c.Status == metav1.ConditionTrue) || !permitted && c.Reason != "RefNotPermitted" {
			t.Fatal(want, c)
		}
	}
}

func TestUnsupportedProtocolListener(t *testing.T) {
	m, st := FromGateway(gateway(listener("tcp", gatewayv1.TCPProtocolType, 5432, ""), listener("http", gatewayv1.HTTPProtocolType, 80, ""), listener("dup", gatewayv1.HTTPSProtocolType, 80, "")), input(), testCfg)
	if c := meta.FindStatusCondition(st.Listeners[0].Conditions, "Accepted"); c.Status != metav1.ConditionFalse || c.Reason != "UnsupportedProtocol" || len(st.Listeners[0].SupportedKinds) != 0 {
		t.Fatal(c)
	}
	if c := meta.FindStatusCondition(st.Listeners[2].Conditions, "Conflicted"); c.Status != metav1.ConditionTrue || c.Reason != "ProtocolConflict" {
		t.Fatal(c)
	}
	if len(m.Listeners) != 1 || m.Listeners[0].Port != 80 || m.Listeners[0].Protocol != "http" {
		t.Fatal(m.Listeners)
	}
	// Some listeners can't be served: the Gateway is accepted, saying which.
	if c := st.Gateway[0]; c.Type != "Accepted" || c.Status != metav1.ConditionTrue || c.Reason != "ListenersNotValid" || !strings.Contains(c.Message, "tcp, dup") {
		t.Fatal(c)
	}
	// None can: the Gateway isn't accepted and has no load balancer, but its listeners say why.
	m, st = FromGateway(gateway(listener("tcp", gatewayv1.TCPProtocolType, 5432, "")), input(), testCfg)
	if c := st.Gateway[0]; m.Name != "" || c.Status != metav1.ConditionFalse || c.Reason != "ListenersNotValid" || len(st.Listeners) != 1 {
		t.Fatal(m.Name, st)
	}
	// A listener whose certificate isn't usable is still accepted (ResolvedRefs says why).
	_, st = FromGateway(gateway(listener("nocert", gatewayv1.HTTPSProtocolType, 443, "")), input(), testCfg)
	if c := st.Gateway[0]; c.Status != metav1.ConditionTrue || c.Reason != "Accepted" {
		t.Fatal(c)
	}
}

// Attachment counts on a listener that can't be served (a missing Secret): AttachedRoutes is
// allowedRoutes and parentRefs alone. Such a listener's routes get no rules, even on a port a
// valid listener shares.
func TestAttachedRoutesOnInvalidListener(t *testing.T) {
	tls := allNamespaces(listener("tls", gatewayv1.HTTPSProtocolType, 443, ""))
	tls.TLS = &gatewayv1.ListenerTLSConfig{CertificateRefs: []gatewayv1.SecretObjectReference{{Name: "does-not-exist"}}}
	clash := allNamespaces(listener("clash", gatewayv1.HTTPSProtocolType, 80, "clash.example.com"))
	m, st := FromGateway(gateway(allNamespaces(listener("http", gatewayv1.HTTPProtocolType, 80, "")), tls, clash),
		input(route("shop", "web", 1, nil, rule(nil, backendRef("", "missing", 80)))), testCfg)
	if st.Listeners[1].AttachedRoutes != 1 || st.Listeners[2].AttachedRoutes != 1 || st.Valid["tls"] || st.Valid["clash"] {
		t.Fatal(st.Listeners)
	}
	if c := parentCond(t, st, "shop", "web", "Accepted"); c.Status != metav1.ConditionTrue {
		t.Fatal(c)
	}
	if len(m.Listeners) != 1 || describe(m.Listeners[0].Rules) != "[][{prefix /}]>500" {
		t.Fatal(m.Listeners)
	}
}

// A Gateway with no listener it can serve makes nothing, and its status says so.
func TestNoValidListenersNotAccepted(t *testing.T) {
	r, k, _, f := newGatewayReconciler(t, gateway(listener("tcp", gatewayv1.TCPProtocolType, 5432, "")))
	got, err := reconcileGateway(t, r, k)
	if err != nil || len(f.log()) != 0 {
		t.Fatal(err, f.log())
	}
	if c := meta.FindStatusCondition(got.Status.Conditions, "Accepted"); c.Status != metav1.ConditionFalse || c.Reason != "ListenersNotValid" || c.ObservedGeneration != 1 {
		t.Fatal(got.Status.Conditions)
	}
	if c := meta.FindStatusCondition(got.Status.Conditions, "Programmed"); c.Status != metav1.ConditionFalse || c.Reason != "Invalid" {
		t.Fatal(c)
	}
	if len(got.Status.Listeners) != 1 || meta.FindStatusCondition(got.Status.Listeners[0].Conditions, "Accepted").Reason != "UnsupportedProtocol" {
		t.Fatal(got.Status.Listeners)
	}
}

func gatewayScheme(t *testing.T) *runtime.Scheme {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := gatewayv1.Install(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func newGatewayReconciler(t *testing.T, objs ...client.Object) (*GatewayReconciler, client.Client, *events.FakeRecorder, *fakeEZGH) {
	f, c := newFake(t)
	class := &gatewayv1.GatewayClass{ObjectMeta: metav1.ObjectMeta{Name: ClassName}, Spec: gatewayv1.GatewayClassSpec{ControllerName: ControllerName}}
	k := fake.NewClientBuilder().WithScheme(gatewayScheme(t)).WithObjects(append(objs, class)...).
		WithStatusSubresource(&gatewayv1.Gateway{}, &gatewayv1.HTTPRoute{}, &gatewayv1.GatewayClass{}).WithInterceptorFuncs(crdChecked).Build()
	rec := events.NewFakeRecorder(100)
	return &GatewayReconciler{Client: k, Clients: c, Config: func() (config.Config, error) { return testCfg, nil }, Recorder: rec}, k, rec, f
}

func reconcileGateway(t *testing.T, r *GatewayReconciler, k client.Client) (*gatewayv1.Gateway, error) {
	t.Helper()
	_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "infra", Name: "edge"}})
	var gw gatewayv1.Gateway
	if gerr := k.Get(ctx, types.NamespacedName{Namespace: "infra", Name: "edge"}, &gw); gerr != nil {
		return nil, err
	}
	return &gw, err
}

func TestProgrammedOnlyWhenActive(t *testing.T) {
	gw := gateway(allNamespaces(listener("http", gatewayv1.HTTPProtocolType, 80, "")))
	rt := route("shop", "web", 1, nil, rule(nil, backendRef("", "web", 80)))
	r, k, _, f := newGatewayReconciler(t, gw, &rt, svc("shop", "web", port("http", 80)))
	f.lbStatus = "provisioning"
	got, err := reconcileGateway(t, r, k)
	if err != nil {
		t.Fatal(err)
	}
	if c := meta.FindStatusCondition(got.Status.Conditions, "Programmed"); c == nil || c.Status != metav1.ConditionFalse || c.Reason != "Pending" || len(got.Status.Addresses) != 0 {
		t.Fatal(got.Status)
	}
	if c := meta.FindStatusCondition(got.Status.Conditions, "Accepted"); c == nil || c.Status != metav1.ConditionTrue || got.Finalizers[0] != lb.Finalizer {
		t.Fatal(got.Status)
	}
	for _, lbr := range f.lbs {
		lbr["status"] = "active"
	}
	if got, err = reconcileGateway(t, r, k); err != nil {
		t.Fatal(err)
	}
	if c := meta.FindStatusCondition(got.Status.Conditions, "Programmed"); c.Status != metav1.ConditionTrue || len(got.Status.Addresses) != 2 || got.Status.Addresses[0].Value != "203.0.113.9" {
		t.Fatal(got.Status)
	}
	if c := meta.FindStatusCondition(got.Status.Listeners[0].Conditions, "Programmed"); c.Status != metav1.ConditionTrue || got.Status.Listeners[0].AttachedRoutes != 1 {
		t.Fatal(got.Status.Listeners)
	}
	var rs gatewayv1.HTTPRoute
	_ = k.Get(ctx, types.NamespacedName{Namespace: "shop", Name: "web"}, &rs)
	if len(rs.Status.Parents) != 1 || rs.Status.Parents[0].ControllerName != ControllerName || meta.FindStatusCondition(rs.Status.Parents[0].Conditions, "Accepted").Status != metav1.ConditionTrue {
		t.Fatal(rs.Status)
	}
	// Targets: the Gateway's state feeds the Service's group.
	if b, _ := GatewayBackends(ctx, k, svc("shop", "web", port("http", 80))); len(b) != 1 || b[0].Port != "http" {
		t.Fatal(b)
	}

	// The route detaches: its entry goes; the Gateway goes: everything is torn down.
	rs.Spec.ParentRefs = nil
	if err := k.Update(ctx, &rs); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcileGateway(t, r, k); err != nil {
		t.Fatal(err)
	}
	_ = k.Get(ctx, types.NamespacedName{Namespace: "shop", Name: "web"}, &rs)
	if len(rs.Status.Parents) != 0 {
		t.Fatal(rs.Status.Parents)
	}
	_ = k.Delete(ctx, got)
	// One reconcile per step: deleting asks to be read again; once the load balancer is gone the
	// rest goes and the finalizer with it.
	res, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "infra", Name: "edge"}})
	if err != nil || res.RequeueAfter == 0 {
		t.Fatal(res, err)
	}
	for _, lbr := range f.lbs {
		lbr["status"] = "deleted"
	}
	if g, _ := reconcileGateway(t, r, k); g != nil || len(f.groups) != 0 {
		t.Fatal("not torn down", f.log())
	}
}

// An unchanged route isn't written again, even with the parentRef as the apiserver defaults it in
// status (group and kind filled).
func TestRouteStatusWrittenOnce(t *testing.T) {
	gw := gateway(allNamespaces(listener("http", gatewayv1.HTTPProtocolType, 80, "")))
	rt := route("shop", "web", 1, nil, rule(nil, backendRef("", "web", 80)))
	r, k, _, _ := newGatewayReconciler(t, gw, &rt, svc("shop", "web", port("http", 80)))
	if _, err := reconcileGateway(t, r, k); err != nil {
		t.Fatal(err)
	}
	var got gatewayv1.HTTPRoute
	_ = k.Get(ctx, types.NamespacedName{Namespace: "shop", Name: "web"}, &got)
	got.Status.Parents[0].ParentRef.Group = ptr.To(gatewayv1.Group(GatewayGroup))
	got.Status.Parents[0].ParentRef.Kind = ptr.To(gatewayv1.Kind("Gateway"))
	for i := range got.Status.Parents[0].Conditions {
		// An old transition time, which a lookup that misses the entry would replace.
		got.Status.Parents[0].Conditions[i].LastTransitionTime = metav1.NewTime(time.Unix(1e9, 0))
	}
	other := gatewayv1.RouteParentStatus{ParentRef: gatewayv1.ParentReference{Name: "theirs"}, ControllerName: "example.com/other",
		Conditions: []metav1.Condition{{Type: "Accepted", Status: metav1.ConditionTrue, Reason: "Accepted", LastTransitionTime: metav1.NewTime(time.Unix(1e9, 0))}}}
	got.Status.Parents = append([]gatewayv1.RouteParentStatus{other}, got.Status.Parents...)
	if err := k.Status().Update(ctx, &got); err != nil {
		t.Fatal(err)
	}
	_ = k.Get(ctx, types.NamespacedName{Namespace: "shop", Name: "web"}, &got)
	rv := got.ResourceVersion
	for range 2 {
		if _, err := reconcileGateway(t, r, k); err != nil {
			t.Fatal(err)
		}
	}
	_ = k.Get(ctx, types.NamespacedName{Namespace: "shop", Name: "web"}, &got)
	if got.ResourceVersion != rv || len(got.Status.Parents) != 2 || got.Status.Parents[0].ControllerName != "example.com/other" {
		t.Fatal(got.ResourceVersion, rv, got.Status.Parents)
	}
}

// Nothing is deleted because something is missing: a Gateway whose GatewayClass is gone keeps its
// load balancer.
func TestMissingGatewayClassKeepsServing(t *testing.T) {
	gw := gateway(allNamespaces(listener("http", gatewayv1.HTTPProtocolType, 80, "")))
	r, k, _, f := newGatewayReconciler(t, gw)
	if _, err := reconcileGateway(t, r, k); err != nil {
		t.Fatal(err)
	}
	var gc gatewayv1.GatewayClass
	_ = k.Get(ctx, types.NamespacedName{Name: ClassName}, &gc)
	if err := k.Delete(ctx, &gc); err != nil {
		t.Fatal(err)
	}
	f.reset()
	got, err := reconcileGateway(t, r, k)
	if err != nil || len(f.log()) != 0 || len(got.Finalizers) != 1 {
		t.Fatal(err, f.log())
	}
}

func TestServiceAndSecretMapToTheirGateways(t *testing.T) {
	https := allNamespaces(listener("https", gatewayv1.HTTPSProtocolType, 443, ""))
	https.TLS = &gatewayv1.ListenerTLSConfig{CertificateRefs: []gatewayv1.SecretObjectReference{{Name: "tls"}}}
	gw := gateway(https)
	other := gateway(listener("http", gatewayv1.HTTPProtocolType, 80, ""))
	other.Name = "other"
	rt := route("shop", "web", 1, nil, rule(nil, backendRef("", "web", 80)))
	_, k, _, _ := newGatewayReconciler(t, gw, other, &rt)
	edge := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "infra", Name: "edge"}}
	if got := ServiceGateways(k)(ctx, svc("shop", "web")); len(got) != 1 || got[0] != edge {
		t.Fatal(got)
	}
	if got := ServiceGateways(k)(ctx, svc("shop", "api")); len(got) != 0 {
		t.Fatal(got)
	}
	if got := SecretGateways(k)(ctx, tlsSecret(t, "infra", "tls", "x")); len(got) != 1 || got[0] != edge {
		t.Fatal(got)
	}
}

func TestObservedGenerationBumps(t *testing.T) {
	gw := gateway(allNamespaces(listener("http", gatewayv1.HTTPProtocolType, 80, "")))
	r, k, _, _ := newGatewayReconciler(t, gw)
	got, err := reconcileGateway(t, r, k)
	if err != nil || meta.FindStatusCondition(got.Status.Conditions, "Accepted").ObservedGeneration != 1 {
		t.Fatal(err, got.Status)
	}
	got.Spec.Listeners = append(got.Spec.Listeners, allNamespaces(listener("alt", gatewayv1.HTTPProtocolType, 8080, "")))
	got.Generation = 2 // the fake client doesn't bump it
	if err := k.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	if got, err = reconcileGateway(t, r, k); err != nil {
		t.Fatal(err)
	}
	for _, c := range got.Status.Conditions {
		if c.ObservedGeneration != 2 {
			t.Fatal(c)
		}
	}
	for _, l := range got.Status.Listeners {
		for _, c := range l.Conditions {
			if c.ObservedGeneration != 2 {
				t.Fatal(l.Name, c)
			}
		}
	}
}

// Review Focus 1: a refused create is a Warning on the Gateway naming the action, an error to
// requeue, and nothing deleted.
func TestGatewayDeniedGetsEvent(t *testing.T) {
	gw := gateway(allNamespaces(listener("http", gatewayv1.HTTPProtocolType, 80, "")))
	r, k, rec, f := newGatewayReconciler(t, gw)
	f.fail["POST /load-balancers"] = [2]string{"403", "access_denied"}
	got, err := reconcileGateway(t, r, k)
	if err == nil {
		t.Fatal("no error to requeue")
	}
	if ev := <-rec.Events; !strings.HasPrefix(ev, "Warning MissingPermission") || !strings.Contains(ev, "lb.loadBalancers.create") {
		t.Fatal(ev)
	}
	if c := meta.FindStatusCondition(got.Status.Conditions, "Programmed"); c.Status != metav1.ConditionFalse || !strings.Contains(c.Message, "lb.loadBalancers.create") {
		t.Fatal(c)
	}
	for _, w := range f.log() {
		if strings.HasPrefix(w, "DELETE") {
			t.Fatal(f.log())
		}
	}
}

func TestGatewayClassAccepted(t *testing.T) {
	_, k, _, _ := newGatewayReconciler(t)
	r := &GatewayClassReconciler{Client: k}
	if _, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: ClassName}}); err != nil {
		t.Fatal(err)
	}
	var gc gatewayv1.GatewayClass
	_ = k.Get(ctx, types.NamespacedName{Name: ClassName}, &gc)
	if c := meta.FindStatusCondition(gc.Status.Conditions, "Accepted"); c == nil || c.Status != metav1.ConditionTrue || len(gc.Status.SupportedFeatures) != len(SupportedFeatures) {
		t.Fatal(gc.Status)
	}
	for i := 1; i < len(SupportedFeatures); i++ {
		if SupportedFeatures[i-1].Name >= SupportedFeatures[i].Name {
			t.Fatal("supportedFeatures must be sorted", SupportedFeatures[i])
		}
	}
}

// spec.addresses isn't accepted, and changes nothing that serves.
func TestUnsupportedAddressKeepsServing(t *testing.T) {
	gw := gateway(allNamespaces(listener("http", gatewayv1.HTTPProtocolType, 80, "")))
	r, k, _, f := newGatewayReconciler(t, gw)
	got, err := reconcileGateway(t, r, k)
	if err != nil {
		t.Fatal(err)
	}
	got.Spec.Addresses = []gatewayv1.GatewaySpecAddress{{Value: "198.51.100.1"}}
	if err := k.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	f.reset()
	if got, err = reconcileGateway(t, r, k); err != nil {
		t.Fatal(err)
	}
	if c := meta.FindStatusCondition(got.Status.Conditions, "Accepted"); c.Status != metav1.ConditionFalse || c.Reason != "UnsupportedAddress" || len(f.log()) != 0 {
		t.Fatal(c, f.log())
	}
}
