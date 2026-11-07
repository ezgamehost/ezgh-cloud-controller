package l7

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/cloud/config"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/cloud/lb"
)

// CertificatesOption names existing certificates on an HTTPS listener: tls.options
// ["lb.ezghcloud.com/certificates"] = "crt_a,crt_b". They come before imported Secrets.
const CertificatesOption = gatewayv1.AnnotationKey(CertificatesAnnotation)

// GatewayGroup is the API group of Gateway API's kinds.
const GatewayGroup = gatewayv1.GroupName

// SupportedFeatures are what the ezgh GatewayClass serves, sorted by name: the core features, and
// the extended ones lb-api renders as Gateway API defines them.
var SupportedFeatures = []gatewayv1.SupportedFeature{
	{Name: "Gateway"}, {Name: "HTTPRoute"}, {Name: "HTTPRoute307RedirectStatusCode"}, {Name: "HTTPRoute308RedirectStatusCode"},
	{Name: "HTTPRouteMethodMatching"}, {Name: "HTTPRoutePathRedirect"}, {Name: "HTTPRoutePortRedirect"}, {Name: "HTTPRouteQueryParamMatching"},
	{Name: "HTTPRouteResponseHeaderModification"}, {Name: "HTTPRouteSchemeRedirect"}, {Name: "ReferenceGrant"},
}

// GatewayInput is what a Gateway's model reads besides the Gateway: every HTTPRoute and
// ReferenceGrant in the cluster, and the Services, TLS Secrets and namespace labels they name, by
// "<ns>/<name>" (namespaces by name). Missing ones are left out.
type GatewayInput struct {
	Routes     []gatewayv1.HTTPRoute
	Grants     []gatewayv1.ReferenceGrant
	Services   map[string]*corev1.Service
	Secrets    map[string]*corev1.Secret
	Namespaces map[string]map[string]string
}

// Status is a Gateway's conditions as its model sees them: the Gateway's Accepted, each listener's
// (Programmed is the reconciler's, once the load balancer is active), and this Gateway's entries
// in each attached route's status. Refused are lb-api's limits, for events on the Gateway.
type Status struct {
	Gateway   []metav1.Condition
	Listeners []gatewayv1.ListenerStatus
	// Valid names the listeners in the model, so they can be Programmed.
	Valid   map[gatewayv1.SectionName]bool
	Routes  map[types.NamespacedName][]gatewayv1.RouteParentStatus
	Refused []lb.Refusal
}

func condition(t string, ok bool, reason, msg string, gen int64) metav1.Condition {
	s := metav1.ConditionFalse
	if ok {
		s = metav1.ConditionTrue
	}
	return metav1.Condition{Type: t, Status: s, Reason: reason, Message: msg, ObservedGeneration: gen}
}

// gwListener is a Gateway listener while the model is built.
type gwListener struct {
	spec    gatewayv1.Listener
	status  *gatewayv1.ListenerStatus
	valid   bool
	certs   []string
	secrets []string
	kinds   bool // allows HTTPRoute
	gwNS    string
}

// FromGateway is the load balancer gw wants, k8s-gw-<ns>-<name>, and its status. HTTP and HTTPS
// listeners become lb listeners (several on one port with different hostnames share one); TLS,
// TCP and UDP listeners aren't accepted. Each attached HTTPRoute's rules go on its listeners'
// ports, with the listener's hostname intersected into every rule's hosts. An unresolved
// backend is a fixed 500; an unsupported match or filter drops its rule (the route is
// PartiallyInvalid, or not accepted when nothing is left).
func FromGateway(gw *gatewayv1.Gateway, in GatewayInput, cfg config.Config) (Model, Status) {
	gen, ref := gw.Generation, gw.Namespace+"/"+gw.Name
	m := Model{Key: "gw-" + string(gw.UID), Name: GatewayLBName(gw), Tags: lb.Tags(cfg, ref), Secrets: map[string]*corev1.Secret{}}
	st := Status{Routes: map[types.NamespacedName][]gatewayv1.RouteParentStatus{}, Valid: map[gatewayv1.SectionName]bool{}}
	if len(gw.Spec.Addresses) > 0 {
		st.Gateway = []metav1.Condition{condition("Accepted", false, "UnsupportedAddress", "EZGH load balancers choose their own addresses; spec.addresses isn't supported", gen)}
		return Model{}, st
	}
	listeners := make([]*gwListener, len(gw.Spec.Listeners))
	st.Listeners = make([]gatewayv1.ListenerStatus, len(gw.Spec.Listeners))
	protocolOf, hostsOn := map[gatewayv1.PortNumber]gatewayv1.ProtocolType{}, map[string]bool{}
	var notValid []string // listeners not accepted, or conflicted
	for i, spec := range gw.Spec.Listeners {
		ls := &st.Listeners[i]
		l := &gwListener{spec: spec, status: ls, valid: true, kinds: true, gwNS: gw.Namespace}
		listeners[i] = l
		ls.Name, ls.SupportedKinds = spec.Name, []gatewayv1.RouteGroupKind{{Group: ptr.To(gatewayv1.Group(GatewayGroup)), Kind: "HTTPRoute"}}
		accepted := condition("Accepted", true, "Accepted", "", gen)
		resolved := condition("ResolvedRefs", true, "ResolvedRefs", "", gen)
		conflicted := condition("Conflicted", false, "NoConflicts", "", gen)
		switch {
		case spec.Protocol != gatewayv1.HTTPProtocolType && spec.Protocol != gatewayv1.HTTPSProtocolType:
			accepted = condition("Accepted", false, "UnsupportedProtocol", fmt.Sprintf("EZGH load balancers serve HTTP and HTTPS listeners through Gateway API; %s isn't supported", spec.Protocol), gen)
			ls.SupportedKinds, l.valid, l.kinds = []gatewayv1.RouteGroupKind{}, false, false
		case protocolOf[spec.Port] != "" && protocolOf[spec.Port] != spec.Protocol:
			conflicted = condition("Conflicted", true, "ProtocolConflict", fmt.Sprintf("Port %d is already %s", spec.Port, protocolOf[spec.Port]), gen)
			l.valid = false
		case hostsOn[fmt.Sprint(spec.Port, "/", ptr.Deref(spec.Hostname, ""))]:
			conflicted = condition("Conflicted", true, "HostnameConflict", fmt.Sprintf("Another listener on port %d has the same hostname", spec.Port), gen)
			l.valid = false
		}
		if l.valid {
			protocolOf[spec.Port] = spec.Protocol
			hostsOn[fmt.Sprint(spec.Port, "/", ptr.Deref(spec.Hostname, ""))] = true
		}
		if l.kinds && spec.AllowedRoutes != nil && len(spec.AllowedRoutes.Kinds) > 0 {
			ls.SupportedKinds = []gatewayv1.RouteGroupKind{}
			l.kinds = false
			for _, k := range spec.AllowedRoutes.Kinds {
				if ptr.Deref(k.Group, GatewayGroup) == GatewayGroup && k.Kind == "HTTPRoute" {
					ls.SupportedKinds, l.kinds = []gatewayv1.RouteGroupKind{{Group: ptr.To(gatewayv1.Group(GatewayGroup)), Kind: "HTTPRoute"}}, true
				} else {
					resolved = condition("ResolvedRefs", false, "InvalidRouteKinds", fmt.Sprintf("%s isn't a route kind EZGH load balancers serve", k.Kind), gen)
				}
			}
		}
		if l.kinds && spec.Protocol == gatewayv1.HTTPSProtocolType {
			if why, reason := l.tls(gw, in, &m); why != "" {
				resolved = condition("ResolvedRefs", false, reason, why, gen)
			}
			if len(l.certs)+len(l.secrets) == 0 {
				l.valid = false
				if resolved.Status == metav1.ConditionTrue {
					resolved = condition("ResolvedRefs", false, "InvalidCertificateRef", "An HTTPS listener needs a certificate: tls.certificateRefs or tls.options[\""+string(CertificatesOption)+"\"]", gen)
				}
			}
		}
		ls.Conditions = []metav1.Condition{accepted, resolved, conflicted}
		if accepted.Status == metav1.ConditionFalse || conflicted.Status == metav1.ConditionTrue {
			notValid = append(notValid, string(spec.Name))
		}
		st.Valid[spec.Name] = l.valid && l.kinds
	}

	// A Secret with no DNS names serves only as its port's one certificate (soleNameless).
	portCerts, portSecrets := map[gatewayv1.PortNumber][]string{}, map[gatewayv1.PortNumber][]string{}
	for _, l := range listeners {
		if st.Valid[l.spec.Name] {
			portCerts[l.spec.Port] = appendNew(portCerts[l.spec.Port], l.certs...)
			portSecrets[l.spec.Port] = appendNew(portSecrets[l.spec.Port], l.secrets...)
		}
	}
	for _, l := range listeners {
		if !st.Valid[l.spec.Name] {
			continue
		}
		keep, had := soleNameless(portCerts[l.spec.Port], portSecrets[l.spec.Port], m.Secrets), len(l.secrets)
		l.secrets = slices.DeleteFunc(l.secrets, func(ref string) bool {
			if ref == keep || !nameless(m.Secrets[ref]) {
				return false
			}
			// The first problem stays the reason.
			if i := slices.IndexFunc(l.status.Conditions, func(c metav1.Condition) bool { return c.Type == "ResolvedRefs" }); l.status.Conditions[i].Status == metav1.ConditionTrue {
				l.status.Conditions[i] = condition("ResolvedRefs", false, "InvalidCertificateRef",
					fmt.Sprintf("Secret %s has no DNS names, so it can only be port %d's one certificate, and the port has others", ref, l.spec.Port), gen)
			}
			return true
		})
		if len(l.secrets) < had && len(l.certs)+len(l.secrets) == 0 {
			l.valid, st.Valid[l.spec.Name] = false, false
		}
	}

	routes := slices.Clone(in.Routes)
	slices.SortFunc(routes, func(a, b gatewayv1.HTTPRoute) int {
		return cmp.Or(a.CreationTimestamp.Compare(b.CreationTimestamp.Time), cmp.Compare(a.Namespace+"/"+a.Name, b.Namespace+"/"+b.Name))
	})
	byPort := map[gatewayv1.PortNumber]*Listener{}
	for _, l := range listeners {
		if !st.Valid[l.spec.Name] {
			continue
		}
		lst := byPort[l.spec.Port]
		if lst == nil {
			lst = &Listener{Port: int(l.spec.Port), Protocol: strings.ToLower(string(l.spec.Protocol)), Default: fixed(404)}
			byPort[l.spec.Port] = lst
		}
		lst.Certificates = appendNew(lst.Certificates, l.certs...)
		lst.Secrets = appendNew(lst.Secrets, l.secrets...)
	}
	seen := map[string]bool{}
	for i := range routes {
		route := &routes[i]
		key := types.NamespacedName{Namespace: route.Namespace, Name: route.Name}
		rules, partial, refs := routeRules(route, in, &m)
		dropped := len(rules) == 0 && partial != "" // not accepted anywhere, so counted nowhere
		attachedTo := map[gatewayv1.SectionName]bool{}
		for _, pr := range route.Spec.ParentRefs {
			if !refersTo(pr, route.Namespace, gw) {
				continue
			}
			ps := gatewayv1.RouteParentStatus{ParentRef: pr, ControllerName: ControllerName}
			accepted := attach(pr, route, listeners, in.Namespaces, func(l *gwListener, hosts []string) {
				if !dropped && !attachedTo[l.spec.Name] {
					attachedTo[l.spec.Name] = true
					l.status.AttachedRoutes++
				}
				lst, once := byPort[l.spec.Port], fmt.Sprint(key, l.spec.Port, hosts)
				if lst == nil || !st.Valid[l.spec.Name] || seen[once] {
					return // an invalid listener serves nothing; two parentRefs to a port add rules once
				}
				seen[once] = true
				for _, r := range rules {
					r.Hosts = hosts
					lst.Rules = append(lst.Rules, splitHosts(r)...)
				}
			}, route.Generation)
			if accepted.Status == metav1.ConditionTrue && dropped {
				accepted = condition("Accepted", false, "UnsupportedValue", partial, route.Generation)
			}
			ps.Conditions = []metav1.Condition{accepted, refs}
			if partial != "" && len(rules) > 0 {
				ps.Conditions = append(ps.Conditions, condition("PartiallyInvalid", true, "UnsupportedValue", partial, route.Generation))
			}
			st.Routes[key] = append(st.Routes[key], ps)
		}
	}
	// On a port several listeners share, a request for one listener's hostname that none of its
	// routes match is a 404, never a route of a hostless or wider listener on the port.
	perPort := map[gatewayv1.PortNumber]int{}
	for _, l := range listeners {
		if st.Valid[l.spec.Name] {
			perPort[l.spec.Port]++
		}
	}
	for _, l := range listeners {
		if lst := byPort[l.spec.Port]; lst != nil && st.Valid[l.spec.Name] && l.spec.Hostname != nil && perPort[l.spec.Port] > 1 {
			lst.Rules = append(lst.Rules, Rule{Source: "Gateway listener " + string(l.spec.Name), Hosts: []string{strings.ToLower(string(*l.spec.Hostname))}, Action: fixed(404)})
		}
	}
	for _, p := range slices.Sorted(maps.Keys(byPort)) {
		m.Listeners = append(m.Listeners, *byPort[p])
	}
	st.Refused = m.limit()
	for ref := range m.Secrets {
		if !slices.ContainsFunc(m.Listeners, func(l Listener) bool { return slices.Contains(l.Secrets, ref) }) {
			delete(m.Secrets, ref)
		}
	}
	switch why := "Listeners not valid: " + strings.Join(notValid, ", "); {
	case len(notValid) == 0:
		st.Gateway = []metav1.Condition{condition("Accepted", true, "Accepted", "", gen)}
	case len(notValid) < len(gw.Spec.Listeners):
		st.Gateway = []metav1.Condition{condition("Accepted", true, "ListenersNotValid", why, gen)}
	default:
		// Not accepted: what exists keeps serving (as for spec.addresses), nothing new is made.
		st.Gateway = []metav1.Condition{condition("Accepted", false, "ListenersNotValid", why, gen)}
		return Model{}, st
	}
	return m, st
}

// GatewayLBName is a Gateway's load balancer's name.
func GatewayLBName(gw *gatewayv1.Gateway) string {
	return cut("k8s-gw-" + gw.Namespace + "-" + gw.Name)
}

func appendNew(to []string, vs ...string) []string {
	for _, v := range vs {
		if !slices.Contains(to, v) {
			to = append(to, v)
		}
	}
	return to
}

// tls reads an HTTPS listener's certificates: crt_ IDs in its options, and Secrets in its
// certificateRefs (another namespace's only with a ReferenceGrant). It returns why a reference
// isn't usable, with the reason, or "".
func (l *gwListener) tls(gw *gatewayv1.Gateway, in GatewayInput, m *Model) (string, string) {
	t := l.spec.TLS
	if t == nil {
		return "", ""
	}
	if ptr.Deref(t.Mode, gatewayv1.TLSModeTerminate) != gatewayv1.TLSModeTerminate {
		l.valid = false
		return "EZGH load balancers terminate TLS; Passthrough isn't supported", "InvalidCertificateRef"
	}
	var why, reason string
	for _, id := range strings.Split(string(t.Options[CertificatesOption]), ",") {
		if id = strings.TrimSpace(id); id == "" {
			continue
		}
		if !strings.HasPrefix(id, "crt_") {
			why, reason = id+" isn't a crt_ ID", "InvalidCertificateRef"
			continue
		}
		l.certs = appendNew(l.certs, id)
	}
	for _, c := range t.CertificateRefs {
		ns := string(ptr.Deref(c.Namespace, gatewayv1.Namespace(gw.Namespace)))
		ref := ns + "/" + string(c.Name)
		switch {
		case ptr.Deref(c.Group, "") != "" || ptr.Deref(c.Kind, "Secret") != "Secret":
			why, reason = fmt.Sprintf("certificateRef %s isn't a Secret", c.Name), "InvalidCertificateRef"
		case ns != gw.Namespace && !granted(in.Grants, GatewayGroup, "Gateway", gw.Namespace, "", "Secret", ns, string(c.Name)):
			why, reason = fmt.Sprintf("Secret %s is in another namespace, and no ReferenceGrant there allows it", ref), "RefNotPermitted"
		default:
			if bad := invalidTLS(in.Secrets[ref]); bad != "" {
				why, reason = "Secret "+ref+" "+bad, "InvalidCertificateRef"
				continue
			}
			m.Secrets[ref] = in.Secrets[ref]
			l.secrets = appendNew(l.secrets, ref)
		}
	}
	return why, reason
}

// granted reports whether a ReferenceGrant in toNS lets fromKind objects in fromNS refer to the
// toKind object name there.
func granted(grants []gatewayv1.ReferenceGrant, fromGroup, fromKind, fromNS, toGroup, toKind, toNS, name string) bool {
	for _, g := range grants {
		if g.Namespace != toNS {
			continue
		}
		from := slices.ContainsFunc(g.Spec.From, func(f gatewayv1.ReferenceGrantFrom) bool {
			return string(f.Group) == fromGroup && string(f.Kind) == fromKind && string(f.Namespace) == fromNS
		})
		to := slices.ContainsFunc(g.Spec.To, func(t gatewayv1.ReferenceGrantTo) bool {
			return string(t.Group) == toGroup && string(t.Kind) == toKind && (t.Name == nil || string(*t.Name) == name)
		})
		if from && to {
			return true
		}
	}
	return false
}

// refersTo reports whether a route's parentRef names gw.
func refersTo(pr gatewayv1.ParentReference, routeNS string, gw *gatewayv1.Gateway) bool {
	return ptr.Deref(pr.Group, GatewayGroup) == GatewayGroup && ptr.Deref(pr.Kind, "Gateway") == "Gateway" &&
		string(ptr.Deref(pr.Namespace, gatewayv1.Namespace(routeNS))) == gw.Namespace && string(pr.Name) == gw.Name
}

// attach attaches route through one parentRef to every listener it may use, calling add with
// each one's hosts, and returns the parentRef's Accepted condition. Attachment is allowedRoutes,
// parentRefs and hostnames only: a listener that can't be served (no certificate, conflicted)
// still counts its routes, as Gateway API's attachedRoutes requires.
func attach(pr gatewayv1.ParentReference, route *gatewayv1.HTTPRoute, listeners []*gwListener, nsLabels map[string]map[string]string,
	add func(*gwListener, []string), gen int64) metav1.Condition {
	matched, allowed, attached := false, false, false
	for _, l := range listeners {
		if pr.SectionName != nil && *pr.SectionName != l.spec.Name || pr.Port != nil && *pr.Port != l.spec.Port {
			continue
		}
		matched = true
		if !l.kinds || !namespaceAllowed(l.spec.AllowedRoutes, route.Namespace, l.gwNS, nsLabels) {
			continue
		}
		allowed = true
		hosts, ok := intersect(l.spec.Hostname, route.Spec.Hostnames)
		if !ok {
			continue
		}
		attached = true
		add(l, hosts)
	}
	switch {
	case !matched:
		return condition("Accepted", false, "NoMatchingParent", "No listener of the Gateway matches the parentRef's sectionName or port", gen)
	case !allowed:
		return condition("Accepted", false, "NotAllowedByListeners", "No matching listener allows this route", gen)
	case !attached:
		return condition("Accepted", false, "NoMatchingListenerHostname", "No matching listener's hostname intersects the route's hostnames", gen)
	}
	return condition("Accepted", true, "Accepted", "", gen)
}

// namespaceAllowed applies a listener's allowedRoutes.namespaces: Same (the default), All, or a
// label Selector.
func namespaceAllowed(a *gatewayv1.AllowedRoutes, routeNS, gwNS string, nsLabels map[string]map[string]string) bool {
	from := gatewayv1.NamespacesFromSame
	var sel *metav1.LabelSelector
	if a != nil && a.Namespaces != nil {
		from, sel = ptr.Deref(a.Namespaces.From, gatewayv1.NamespacesFromSame), a.Namespaces.Selector
	}
	switch from {
	case gatewayv1.NamespacesFromAll:
		return true
	case gatewayv1.NamespacesFromSelector:
		s, err := metav1.LabelSelectorAsSelector(sel)
		return err == nil && sel != nil && s.Matches(labels.Set(nsLabels[routeNS]))
	case gatewayv1.NamespacesFromSame:
		return routeNS == gwNS
	}
	return false
}

// intersect is the hosts a route's rules get on a listener: the route's hostnames that the
// listener's hostname takes (the narrower of two that overlap), and false when none do.
func intersect(listener *gatewayv1.Hostname, route []gatewayv1.Hostname) ([]string, bool) {
	if len(route) == 0 {
		if listener == nil {
			return nil, true
		}
		return []string{strings.ToLower(string(*listener))}, true
	}
	var out []string
	for _, h := range route {
		r := strings.ToLower(string(h))
		if listener == nil {
			out = appendNew(out, r)
			continue
		}
		l := strings.ToLower(string(*listener))
		switch {
		case l == r || covers(l, r):
			out = appendNew(out, r)
		case covers(r, l):
			out = appendNew(out, l)
		}
	}
	return out, len(out) > 0
}

// covers reports whether wildcard w (*.example.com) takes h: a name or narrower wildcard below it.
func covers(w, h string) bool {
	suffix, ok := strings.CutPrefix(w, "*")
	return ok && strings.HasSuffix(h, suffix) && len(h) > len(suffix)
}

// hopByHop are the headers lb-api refuses in a header modifier (P5).
var hopByHop = []string{"content-length", "transfer-encoding", "connection", "upgrade", "keep-alive", "te"}

// routeRules are a route's lb rules (one per match, without hosts), why some were dropped (""
// when none were), and its ResolvedRefs condition.
func routeRules(route *gatewayv1.HTTPRoute, in GatewayInput, m *Model) ([]Rule, string, metav1.Condition) {
	gen := route.Generation
	refs := condition("ResolvedRefs", true, "ResolvedRefs", "", gen)
	var out []Rule
	var dropped []string
	src := "HTTPRoute " + route.Namespace + "/" + route.Name
	for i, rule := range route.Spec.Rules {
		action, why, refReason, refWhy := ruleAction(route, rule, in, m)
		if refReason != "" && refs.Status == metav1.ConditionTrue {
			refs = condition("ResolvedRefs", false, refReason, refWhy, gen)
		}
		if why != "" {
			dropped = append(dropped, fmt.Sprintf("rule %d: %s", i, why))
			continue
		}
		matches := rule.Matches
		if len(matches) == 0 {
			matches = []gatewayv1.HTTPRouteMatch{{}}
		}
		var rs []Rule
		for _, mt := range matches {
			r, why := matchRule(mt)
			if why == "" {
				r.Source, r.Action = src, action
				why = checkMatch(r)
			}
			if why != "" {
				rs = nil
				dropped = append(dropped, fmt.Sprintf("rule %d: %s", i, why))
				break
			}
			rs = append(rs, r)
		}
		out = append(out, rs...)
	}
	return out, strings.Join(dropped, "; "), refs
}

func matchRule(mt gatewayv1.HTTPRouteMatch) (Rule, string) {
	var r Rule
	p := ptr.Deref(mt.Path, gatewayv1.HTTPPathMatch{})
	v := ptr.Deref(p.Value, "/")
	switch ptr.Deref(p.Type, gatewayv1.PathMatchPathPrefix) {
	case gatewayv1.PathMatchExact:
		r.Paths = []PathMatch{{Type: "exact", Value: v}}
	case gatewayv1.PathMatchPathPrefix:
		if len(v) > 1 {
			v = strings.TrimRight(v, "/")
		}
		r.Paths = []PathMatch{{Type: "prefix", Value: cmp.Or(v, "/")}}
	default:
		return r, "regular expression paths aren't supported"
	}
	if !validPath(v) {
		return r, fmt.Sprintf("path %q isn't printable ASCII without spaces, # or ?", v)
	}
	for _, h := range mt.Headers {
		if ptr.Deref(h.Type, gatewayv1.HeaderMatchExact) != gatewayv1.HeaderMatchExact {
			return r, "regular expression header matches aren't supported"
		}
		r.Headers = append(r.Headers, NameValue{Name: string(h.Name), Value: h.Value})
	}
	for _, q := range mt.QueryParams {
		if ptr.Deref(q.Type, gatewayv1.QueryParamMatchExact) != gatewayv1.QueryParamMatchExact {
			return r, "regular expression query parameter matches aren't supported"
		}
		r.Query = append(r.Query, NameValue{Name: string(q.Name), Value: q.Value})
	}
	if mt.Method != nil {
		r.Methods = []string{strings.ToUpper(string(*mt.Method))}
	}
	return r, ""
}

// ruleAction is a route rule's action: a redirect, or a forward to its resolved backends with its
// header modifiers, or a fixed 500 when none resolve. why is an unsupported filter; refReason and
// refWhy the first unresolved backend.
func ruleAction(route *gatewayv1.HTTPRoute, rule gatewayv1.HTTPRouteRule, in GatewayInput, m *Model) (a Action, why, refReason, refWhy string) {
	var redirect *Redirect
	for _, f := range rule.Filters {
		switch f.Type {
		case gatewayv1.HTTPRouteFilterRequestHeaderModifier:
			if a.RequestHeaders, why = headerModifier(f.RequestHeaderModifier); why != "" {
				return a, why, "", ""
			}
		case gatewayv1.HTTPRouteFilterResponseHeaderModifier:
			if a.ResponseHeaders, why = headerModifier(f.ResponseHeaderModifier); why != "" {
				return a, why, "", ""
			}
		case gatewayv1.HTTPRouteFilterRequestRedirect:
			if redirect, why = redirectOf(f.RequestRedirect); why != "" {
				return a, why, "", ""
			}
		default:
			return a, fmt.Sprintf("the %s filter isn't supported", f.Type), "", ""
		}
	}
	if redirect != nil {
		// shortcut: header modifiers beside a redirect aren't applied (lb-api takes them on forwards only).
		return Action{Type: "redirect", Redirect: redirect}, "", "", ""
	}
	var to []Weighted
	for _, br := range rule.BackendRefs {
		if len(br.Filters) > 0 {
			return a, "backendRef filters aren't supported", "", ""
		}
		key, reason, msg := routeBackend(route, br.BackendObjectReference, in, m)
		if reason != "" {
			if refReason == "" {
				refReason, refWhy = reason, msg
			}
			// shortcut: Gateway API wants an unresolved backend's share of requests answered 500;
			// lb-api can't mix a fixed response into a weighted forward, so that share goes to
			// the resolved backends instead (all unresolved is a 500). Upgrade if lb-api gains it.
			continue
		}
		if w := int(ptr.Deref(br.Weight, 1)); w > 0 {
			to = append(to, Weighted{Backend: key, Weight: w})
		}
	}
	if len(to) == 0 {
		return Action{Type: "fixedResponse", Fixed: &Fixed{StatusCode: 500, ContentType: "text/plain"}}, "", refReason, refWhy
	}
	a.Type, a.Forward = "forward", to
	return a, "", refReason, refWhy
}

// routeBackend resolves a backendRef to a backend of m, or says why it can't.
func routeBackend(route *gatewayv1.HTTPRoute, ref gatewayv1.BackendObjectReference, in GatewayInput, m *Model) (string, string, string) {
	ns := string(ptr.Deref(ref.Namespace, gatewayv1.Namespace(route.Namespace)))
	name := ns + "/" + string(ref.Name)
	switch {
	case ptr.Deref(ref.Group, "") != "" || ptr.Deref(ref.Kind, "Service") != "Service":
		return "", "InvalidKind", fmt.Sprintf("backendRef %s isn't a Service", ref.Name)
	case ns != route.Namespace && !granted(in.Grants, GatewayGroup, "HTTPRoute", route.Namespace, "", "Service", ns, string(ref.Name)):
		return "", "RefNotPermitted", fmt.Sprintf("Service %s is in another namespace, and no ReferenceGrant there allows it", name)
	case in.Services[name] == nil:
		return "", "BackendNotFound", "Service " + name + " doesn't exist"
	case ref.Port == nil:
		return "", "BackendNotFound", "backendRef " + name + " has no port"
	}
	for _, p := range in.Services[name].Spec.Ports {
		if p.Port == int32(*ref.Port) {
			k := BackendKey(ns, string(ref.Name), int(p.Port))
			m.addBackend(Backend{Key: k, Service: name, Port: int(p.Port)})
			return k, "", ""
		}
	}
	return "", "BackendNotFound", fmt.Sprintf("Service %s has no port %d", name, *ref.Port)
}

func headerModifier(f *gatewayv1.HTTPHeaderFilter) (*HeaderModifier, string) {
	if f == nil {
		return nil, ""
	}
	h := &HeaderModifier{Set: []NameValue{}, Add: []NameValue{}, Remove: []string{}}
	for _, s := range f.Set {
		h.Set = append(h.Set, NameValue{Name: string(s.Name), Value: s.Value})
	}
	for _, s := range f.Add {
		h.Add = append(h.Add, NameValue{Name: string(s.Name), Value: s.Value})
	}
	h.Remove = append(h.Remove, f.Remove...)
	if len(h.Set) > MaxHeaderEdits || len(h.Add) > MaxHeaderEdits || len(h.Remove) > MaxHeaderEdits {
		return nil, fmt.Sprintf("a header modifier list has more than %d entries", MaxHeaderEdits)
	}
	names := slices.Concat(h.Remove)
	for _, nv := range slices.Concat(h.Set, h.Add) {
		names = append(names, nv.Name)
	}
	for _, n := range names {
		if slices.Contains(hopByHop, strings.ToLower(n)) {
			return nil, fmt.Sprintf("the %s header can't be modified", n)
		}
	}
	return h, ""
}

func redirectOf(f *gatewayv1.HTTPRequestRedirectFilter) (*Redirect, string) {
	if f == nil {
		return nil, "a RequestRedirect filter needs requestRedirect"
	}
	r := &Redirect{Scheme: f.Scheme, StatusCode: ptr.Deref(f.StatusCode, 302)}
	if !slices.Contains([]int{301, 302, 307, 308}, r.StatusCode) {
		return nil, fmt.Sprintf("redirect status %d isn't supported", r.StatusCode)
	}
	if f.Hostname != nil {
		r.Host = ptr.To(strings.ToLower(string(*f.Hostname))) // lb-api takes lowercase names
	}
	if f.Port != nil {
		r.Port = ptr.To(int(*f.Port))
	}
	if f.Path != nil {
		if f.Path.Type != gatewayv1.FullPathHTTPPathModifier || f.Path.ReplaceFullPath == nil {
			return nil, "only ReplaceFullPath redirects are supported"
		}
		r.Path = f.Path.ReplaceFullPath
	}
	return r, ""
}

// SetGatewayStatus writes model status st onto gw's status, with Programmed (programmed reports
// whether the load balancer is active with every listener made; why says otherwise) and the
// addresses. Transition times stay while a condition's status does.
func SetGatewayStatus(gw *gatewayv1.Gateway, st Status, programmed bool, why string, addrs []string) {
	gen := gw.Generation
	conds := append([]metav1.Condition(nil), st.Gateway...)
	accepted := len(conds) > 0 && conds[0].Status == metav1.ConditionTrue
	switch {
	case !accepted:
		conds = append(conds, condition("Programmed", false, "Invalid", "The Gateway isn't accepted", gen))
	case programmed:
		conds = append(conds, condition("Programmed", true, "Programmed", "", gen))
	default:
		conds = append(conds, condition("Programmed", false, "Pending", why, gen))
	}
	for _, c := range conds {
		meta.SetStatusCondition(&gw.Status.Conditions, c)
	}
	gw.Status.Addresses = nil
	if programmed {
		for _, a := range addrs {
			gw.Status.Addresses = append(gw.Status.Addresses, gatewayv1.GatewayStatusAddress{Type: ptr.To(gatewayv1.IPAddressType), Value: a})
		}
	}
	old := gw.Status.Listeners
	gw.Status.Listeners = nil
	for _, ls := range st.Listeners {
		// Merged, even with no old listener: SetStatusCondition sets the transition time the CRD requires.
		var conds []metav1.Condition
		for _, o := range old {
			if o.Name == ls.Name {
				conds = o.Conditions
			}
		}
		for _, c := range ls.Conditions {
			meta.SetStatusCondition(&conds, c)
		}
		ls.Conditions = conds
		p := condition("Programmed", true, "Programmed", "", gen)
		if !st.Valid[ls.Name] {
			p = condition("Programmed", false, "Invalid", "The listener isn't valid", gen)
		} else if !programmed {
			p = condition("Programmed", false, "Pending", why, gen)
		}
		meta.SetStatusCondition(&ls.Conditions, p)
		gw.Status.Listeners = append(gw.Status.Listeners, ls)
	}
}

// SetRouteParents replaces gw's entries in route's status with want, in place, keeping every
// other controller's and parent's, and each condition's transition time while its status stays,
// so an unchanged route reads back unchanged (and isn't written again).
func SetRouteParents(route *gatewayv1.HTTPRoute, gw *gatewayv1.Gateway, want []gatewayv1.RouteParentStatus) {
	used := make([]bool, len(want))
	merge := func(old []metav1.Condition, w gatewayv1.RouteParentStatus) []metav1.Condition {
		conds := slices.Clone(old)
		for _, c := range w.Conditions {
			meta.SetStatusCondition(&conds, c)
		}
		// A condition no longer reported (PartiallyInvalid) goes.
		return slices.DeleteFunc(conds, func(c metav1.Condition) bool { return meta.FindStatusCondition(w.Conditions, c.Type) == nil })
	}
	out := []gatewayv1.RouteParentStatus{} // the CRD requires parents, even empty
	for _, p := range route.Status.Parents {
		if p.ControllerName != ControllerName || !refersTo(p.ParentRef, route.Namespace, gw) {
			out = append(out, p)
			continue
		}
		for i, w := range want {
			if !used[i] && sameParent(p.ParentRef, w.ParentRef, route.Namespace) {
				used[i] = true
				p.Conditions = merge(p.Conditions, w)
				out = append(out, p)
				break
			}
		}
	}
	for i, w := range want {
		if !used[i] {
			w.Conditions = merge(nil, w)
			out = append(out, w)
		}
	}
	route.Status.Parents = out
}

// sameParent compares parentRefs with their defaults filled in (the apiserver fills group and
// kind; an unset namespace is the route's).
func sameParent(a, b gatewayv1.ParentReference, routeNS string) bool {
	norm := func(p gatewayv1.ParentReference) gatewayv1.ParentReference {
		p.Group = ptr.To(ptr.Deref(p.Group, GatewayGroup))
		p.Kind = ptr.To(ptr.Deref(p.Kind, "Gateway"))
		p.Namespace = ptr.To(ptr.Deref(p.Namespace, gatewayv1.Namespace(routeNS)))
		return p
	}
	return equality.Semantic.DeepEqual(norm(a), norm(b))
}
