package l7

import (
	"cmp"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/cloud/config"
	cloudevents "github.com/ezgamehost/ezgh-cloud-controller/internal/cloud/events"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/cloud/lb"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/cloud/run"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient"
)

// Ingress annotations, beside lb's lb.ezghcloud.com/certificates.
const (
	// GroupAnnotation puts Ingresses of one namespace on one load balancer; the same group name in
	// another namespace is another group (no cross-namespace sharing in v1).
	GroupAnnotation        = lb.Prefix + "group"
	SSLRedirectAnnotation  = lb.Prefix + "ssl-redirect"
	CertificatesAnnotation = lb.Prefix + "certificates"
	legacyClassAnnotation  = "kubernetes.io/ingress.class"
)

// provisioningPoll is how often a load balancer that isn't active yet is read again.
const provisioningPoll = 15 * time.Second

// IsOurs reports whether ing is for the ezgh IngressClass.
func IsOurs(ing *networkingv1.Ingress) bool {
	if ing.Spec.IngressClassName != nil {
		return *ing.Spec.IngressClassName == ClassName
	}
	return ing.Annotations[legacyClassAnnotation] == ClassName
}

// IngressGroup is the group a live ezgh Ingress belongs to, "<ns>/<group>": its namespace and
// lb.ezghcloud.com/group (a DNS label), else its own name. Groups never span namespaces (an
// Ingresses in two namespaces naming one group get two load balancers), and an
// Ingress without the annotation is in the group named after itself. ok is false for an Ingress
// that isn't ours, is being deleted, or names a group that isn't a DNS label.
func IngressGroup(ing *networkingv1.Ingress) (group string, ok bool) {
	if !IsOurs(ing) || ing.DeletionTimestamp != nil {
		return "", false
	}
	g, set := ing.Annotations[GroupAnnotation]
	if !set {
		return ing.Namespace + "/" + ing.Name, true
	}
	return ing.Namespace + "/" + g, len(validation.IsDNS1123Label(g)) == 0
}

// IngressLBName is a group's load balancer's name.
func IngressLBName(group string) string { return cut("k8s-ing-" + strings.ReplaceAll(group, "/", "-")) }

// groupRequest is the reconcile request of a group "<ns>/<group>".
func groupRequest(group string) reconcile.Request {
	ns, name, _ := strings.Cut(group, "/")
	return reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}
}

func requestGroup(req reconcile.Request) string { return req.Namespace + "/" + req.Name }

// sortIngresses orders a group: creation time, then namespace/name.
func sortIngresses(ings []*networkingv1.Ingress) {
	slices.SortFunc(ings, func(a, b *networkingv1.Ingress) int {
		return cmp.Or(a.CreationTimestamp.Compare(b.CreationTimestamp.Time), cmp.Compare(a.Namespace+"/"+a.Name, b.Namespace+"/"+b.Name))
	})
}

// FromIngresses is the load balancer an Ingress group wants, with what it can't serve by Ingress
// ("<ns>/<name>"). Listeners: http 80 always, https 443 once a certificate is named
// (lb.ezghcloud.com/certificates) or imported (spec.tls[].secretName); ssl-redirect makes 80 a
// 301 to https. Paths: Exact is exact, Prefix and ImplementationSpecific are prefixes. A missing
// Service or port is a fixed 503 for its paths; no rule matching is the first defaultBackend, else
// a fixed 404.
func FromIngresses(group string, ings []*networkingv1.Ingress, services map[string]*corev1.Service, secrets map[string]*corev1.Secret, cfg config.Config) (Model, map[string][]lb.Refusal) {
	ings = slices.Clone(ings)
	sortIngresses(ings)
	refused := map[string][]lb.Refusal{}
	m := Model{Name: IngressLBName(group), Tags: lb.Tags(cfg, group), Secrets: map[string]*corev1.Secret{}}
	if len(ings) > 0 {
		m.Key = "ing-" + string(ings[0].UID)
	}
	var certs, secretRefs []string
	namedBy := map[string]string{} // a Secret's first Ingress
	var rules []Rule
	var def *Action
	redirect := false
	for _, ing := range ings {
		key, src := ing.Namespace+"/"+ing.Name, "Ingress "+ing.Namespace+"/"+ing.Name
		refuse := func(reason, format string, args ...any) {
			refused[key] = append(refused[key], lb.Refusal{Reason: reason, Message: fmt.Sprintf(format, args...)})
		}
		for _, id := range strings.Split(ing.Annotations[CertificatesAnnotation], ",") {
			if id = strings.TrimSpace(id); id == "" {
				continue
			}
			if !strings.HasPrefix(id, "crt_") {
				refuse(cloudevents.InvalidAnnotation, "%s: %s isn't a crt_ ID", CertificatesAnnotation, id)
			} else if !slices.Contains(certs, id) {
				certs = append(certs, id)
			}
		}
		if v, ok := ing.Annotations[SSLRedirectAnnotation]; ok {
			b, err := strconv.ParseBool(v)
			if err != nil {
				refuse(cloudevents.InvalidAnnotation, "%s must be true or false", SSLRedirectAnnotation)
			}
			redirect = redirect || b
		}
		for _, t := range ing.Spec.TLS {
			if t.SecretName == "" {
				continue
			}
			ref := ing.Namespace + "/" + t.SecretName
			if why := invalidTLS(secrets[ref]); why != "" {
				refuse(InvalidCertificate, "Secret %s %s", ref, why)
				continue
			}
			m.Secrets[ref] = secrets[ref]
			if !slices.Contains(secretRefs, ref) {
				secretRefs, namedBy[ref] = append(secretRefs, ref), key
			}
		}
		backend := func(b networkingv1.IngressBackend) Action {
			a, why := ingressBackend(&m, ing.Namespace, b, services)
			if why != "" {
				refuse(BackendNotFound, "%s", why)
			}
			return a
		}
		if ing.Spec.DefaultBackend != nil && def == nil {
			a := backend(*ing.Spec.DefaultBackend)
			def = &a
		}
		for _, r := range ing.Spec.Rules {
			if r.HTTP == nil {
				continue
			}
			var hosts []string
			if r.Host != "" {
				hosts = []string{strings.ToLower(r.Host)}
			}
			for _, p := range r.HTTP.Paths {
				pm, ok := ingressPath(p)
				if !ok {
					refuse(cloudevents.InvalidAnnotation, "path %q can't be served: an EZGH rule's path is printable ASCII without spaces, # or ?", p.Path)
					continue
				}
				rules = append(rules, Rule{Source: src, Hosts: hosts, Paths: []PathMatch{pm}, Action: backend(p.Backend)})
			}
		}
	}
	if def == nil {
		a := fixed(404)
		def = &a
	}
	keep := soleNameless(certs, secretRefs, m.Secrets)
	secretRefs = slices.DeleteFunc(secretRefs, func(ref string) bool {
		if ref == keep || !nameless(m.Secrets[ref]) {
			return false
		}
		delete(m.Secrets, ref)
		refused[namedBy[ref]] = append(refused[namedBy[ref]], lb.Refusal{Reason: InvalidCertificate,
			Message: fmt.Sprintf("Secret %s has no DNS names, so it can only be the group's one certificate, and the group has others", ref)})
		return true
	})
	https := len(certs)+len(secretRefs) > 0
	httpListener := Listener{Port: 80, Protocol: "http", Default: *def, Rules: rules}
	if redirect && https {
		httpListener = Listener{Port: 80, Protocol: "http", Default: Action{Type: "redirect", Redirect: &Redirect{Scheme: ptr.To("https"), StatusCode: 301}}}
	} else if redirect && len(ings) > 0 {
		key := ings[0].Namespace + "/" + ings[0].Name
		refused[key] = append(refused[key], lb.Refusal{Reason: cloudevents.InvalidAnnotation, Message: SSLRedirectAnnotation + " needs a certificate: " + CertificatesAnnotation + " or spec.tls"})
	}
	m.Listeners = []Listener{httpListener}
	if https {
		m.Listeners = append(m.Listeners, Listener{Port: 443, Protocol: "https", Certificates: certs, Secrets: secretRefs, Default: *def, Rules: slices.Clone(rules)})
	}
	for _, r := range m.limit() {
		if len(ings) == 0 {
			break
		}
		key := ings[0].Namespace + "/" + ings[0].Name
		refused[key] = append(refused[key], r)
	}
	return m, refused
}

// ingressBackend is a backend's forward, or a fixed 503 and why when its Service or port is gone.
func ingressBackend(m *Model, ns string, b networkingv1.IngressBackend, services map[string]*corev1.Service) (Action, string) {
	if b.Service == nil {
		return fixed(503), "only Service backends are served"
	}
	ref := ns + "/" + b.Service.Name
	svc := services[ref]
	if svc == nil {
		return fixed(503), "Service " + ref + " doesn't exist; its paths answer 503"
	}
	port := 0
	for _, p := range svc.Spec.Ports {
		if b.Service.Port.Name != "" && p.Name == b.Service.Port.Name || b.Service.Port.Name == "" && p.Port == b.Service.Port.Number {
			port = int(p.Port)
		}
	}
	if port == 0 {
		return fixed(503), fmt.Sprintf("Service %s has no port %s; its paths answer 503", ref, cmp.Or(b.Service.Port.Name, strconv.Itoa(int(b.Service.Port.Number))))
	}
	k := BackendKey(ns, b.Service.Name, port)
	m.addBackend(Backend{Key: k, Service: ref, Port: port})
	return forward(Weighted{Backend: k, Weight: 1}), ""
}

// ingressPath is an Ingress path as an lb path. A prefix's trailing slash goes (Ingress ignores it).
func ingressPath(p networkingv1.HTTPIngressPath) (PathMatch, bool) {
	v := cmp.Or(p.Path, "/")
	if !validPath(v) {
		return PathMatch{}, false
	}
	if p.PathType != nil && *p.PathType == networkingv1.PathTypeExact {
		return PathMatch{Type: "exact", Value: v}, true
	}
	if len(v) > 1 {
		v = strings.TrimRight(v, "/")
	}
	return PathMatch{Type: "prefix", Value: cmp.Or(v, "/")}, true
}

// validPath is lb-api's path rule: starts with /, printable ASCII without space, # or ?.
func validPath(v string) bool {
	if !strings.HasPrefix(v, "/") || len(v) > 1024 {
		return false
	}
	for _, r := range v {
		if r <= ' ' || r > '~' || r == '#' || r == '?' {
			return false
		}
	}
	return true
}

// invalidTLS is why s isn't a usable kubernetes.io/tls pair, or "". It never says what's in it.
func invalidTLS(s *corev1.Secret) string {
	switch {
	case s == nil:
		return "doesn't exist (or isn't of type kubernetes.io/tls)"
	case s.Type != corev1.SecretTypeTLS:
		return "isn't of type kubernetes.io/tls"
	}
	if _, err := tls.X509KeyPair(s.Data[corev1.TLSCertKey], s.Data[corev1.TLSPrivateKeyKey]); err != nil {
		return "isn't a valid certificate and key pair"
	}
	return ""
}

// nameless reports whether a usable TLS Secret's certificate has no DNS names. Imported, it covers
// no hostname, and lb-api takes it only as a listener's default certificate (HAProxy would choose
// an SNI certificate by its CN), so it serves only as a port's one certificate.
func nameless(s *corev1.Secret) bool {
	pair, err := tls.X509KeyPair(s.Data[corev1.TLSCertKey], s.Data[corev1.TLSPrivateKeyKey])
	return err == nil && pair.Leaf != nil && len(pair.Leaf.DNSNames) == 0
}

// soleNameless is the Secret with no DNS names that a port's certificates (crt_ IDs) and Secrets
// keep: the first, when nothing else is there to have names; else none, and every one goes.
func soleNameless(certs, refs []string, secrets map[string]*corev1.Secret) string {
	if len(certs) > 0 {
		return ""
	}
	first := ""
	for _, ref := range refs {
		if !nameless(secrets[ref]) {
			return ""
		}
		first = cmp.Or(first, ref)
	}
	return first
}

// IngressReconciler keeps one load balancer per Ingress group. Its requests are groups
// (groupRequest), not Ingresses.
type IngressReconciler struct {
	Client   client.Client
	Clients  *ezghclient.Clients
	Config   func() (config.Config, error)
	Recorder events.EventRecorder
	warned   sync.Map // Ingress UID → last refusals reported
}

// Reconcile converges one group. Its members are the live ezgh Ingresses naming it, all in its
// namespace; its state is on each of them, and on an Ingress that left it (deleted, another class
// or group) until what it held is gone. Each member holds the finalizer while the group exists.
// A member still holding another group's state waits until that group lets it go, so a move
// between groups never loses what the old group made.
func (r *IngressReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	group := requestGroup(req)
	var list networkingv1.IngressList
	if err := r.Client.List(ctx, &list, client.InNamespace(req.Namespace)); err != nil {
		return reconcile.Result{}, err
	}
	var members, holders []*networkingv1.Ingress
	var have State
	waiting := false
	for i := range list.Items {
		ing := &list.Items[i]
		g, ok := IngressGroup(ing)
		if !ok && IsOurs(ing) && ing.DeletionTimestamp == nil && ing.Namespace+"/"+ing.Name == group {
			r.warn(ing, []lb.Refusal{{Reason: cloudevents.InvalidAnnotation, Message: GroupAnnotation + " must be a DNS label (lowercase letters, digits and -); the Ingress isn't served"}})
		}
		st, err := readState(ing.Annotations)
		if err != nil {
			cloudevents.Warn(r.Recorder, ing, cloudevents.InvalidAnnotation, "%v", err)
			return reconcile.Result{}, err
		}
		if ok && g == group {
			members = append(members, ing)
			waiting = waiting || st.Group != "" && st.Group != group && !st.Empty()
		}
		if st.Group == group {
			holders = append(holders, ing)
			// The fullest copy wins: one that holds something, then the latest epoch.
			if st.Empty() != have.Empty() && !st.Empty() || st.Empty() == have.Empty() && st.Epoch > have.Epoch {
				have = st
			}
		}
	}
	if waiting {
		return reconcile.Result{RequeueAfter: provisioningPoll}, nil
	}
	if len(members) == 0 && len(holders) == 0 {
		return reconcile.Result{}, nil
	}
	sortIngresses(members)
	cfg, err := r.Config()
	if err != nil {
		return reconcile.Result{}, err
	}
	want := Model{Tags: lb.Tags(cfg, group), Named: IngressLBName(group)}
	fp := ""
	if len(members) > 0 {
		services, secrets, err := r.inputs(ctx, members)
		if err != nil {
			return reconcile.Result{}, err
		}
		var refused map[string][]lb.Refusal
		want, refused = FromIngresses(group, members, services, secrets, cfg)
		if have.Key != "" {
			want.Key = have.Key // a group keeps its first member's key while it lives
		}
		for _, ing := range members {
			r.warn(ing, refused[ing.Namespace+"/"+ing.Name])
		}
		if fp = ingressFingerprint(members, services, secrets); have.Failed != "" && have.Failed == fp {
			return reconcile.Result{}, nil // its create failed; reported, and retried when the group changes
		}
		have.Failed = ""
		// The finalizer goes on before anything is made, so an Ingress deleted meanwhile can't leak it.
		for _, ing := range members {
			if !controllerutil.ContainsFinalizer(ing, lb.Finalizer) {
				orig := ing.DeepCopy()
				controllerutil.AddFinalizer(ing, lb.Finalizer)
				if err := r.Client.Patch(ctx, ing, client.MergeFrom(orig)); err != nil {
					return reconcile.Result{}, err
				}
			}
		}
	}
	if want.Key == "" {
		want.Key = have.Key
	}
	st, applyErr := Apply(ctx, r.Clients, cfg, have, want)
	st.Group, st.Key = group, want.Key
	var failed *lb.CreateFailedError
	if errors.As(applyErr, &failed) {
		st.Failed = fp
	}
	if err := r.save(ctx, group, members, holders, st); err != nil {
		return reconcile.Result{}, err
	}
	report := members
	if len(report) == 0 {
		report = holders
	}
	for _, ing := range report {
		lb.WarnForged(r.Recorder, ing, st.Forged)
	}
	if failed != nil {
		for _, ing := range report {
			cloudevents.Warn(r.Recorder, ing, cloudevents.EZGHAPIError, "%s. Change the Ingress to try again.", failed.Error())
		}
		return reconcile.Result{}, nil
	}
	if d, ok := run.Busy(applyErr); ok {
		return reconcile.Result{RequeueAfter: d}, nil
	}
	if applyErr != nil {
		// Review Focus 1: every Ingress of the group says why; nothing was deleted for a refused read.
		// shortcut: a Retry-After is honoured only for an Ingress's own group (run.Fail keys it by
		// the object), not a named group's; the backoff still applies.
		for _, ing := range report[1:] {
			cloudevents.Report(r.Recorder, ing, applyErr)
		}
		return run.Fail(r.Recorder, report[0], applyErr)
	}
	if len(members) > 0 && st.Status != "active" || len(members) == 0 && !st.Empty() {
		// Provisioning, or tearing down: read again until it's active or gone.
		return reconcile.Result{RequeueAfter: provisioningPoll}, nil
	}
	return reconcile.Result{}, nil
}

// inputs are the Services and TLS Secrets the members name, from the cache; missing ones are left
// out (FromIngresses reports them).
func (r *IngressReconciler) inputs(ctx context.Context, members []*networkingv1.Ingress) (map[string]*corev1.Service, map[string]*corev1.Secret, error) {
	services, secrets := map[string]*corev1.Service{}, map[string]*corev1.Secret{}
	for _, ing := range members {
		for _, name := range ingressServices(ing) {
			var svc corev1.Service
			if err := getIfExists(ctx, r.Client, ing.Namespace, name, &svc); err != nil {
				return nil, nil, err
			} else if svc.Name != "" {
				services[ing.Namespace+"/"+name] = &svc
			}
		}
		for _, t := range ing.Spec.TLS {
			var s corev1.Secret
			if t.SecretName == "" {
				continue
			}
			if err := getIfExists(ctx, r.Client, ing.Namespace, t.SecretName, &s); err != nil {
				return nil, nil, err
			} else if s.Name != "" {
				secrets[ing.Namespace+"/"+t.SecretName] = &s
			}
		}
	}
	return services, secrets, nil
}

func getIfExists(ctx context.Context, c client.Reader, ns, name string, obj client.Object) error {
	return client.IgnoreNotFound(c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, obj))
}

// ingressServices are the Service names ing's backends name.
func ingressServices(ing *networkingv1.Ingress) []string {
	var out []string
	add := func(b *networkingv1.IngressBackend) {
		if b != nil && b.Service != nil && !slices.Contains(out, b.Service.Name) {
			out = append(out, b.Service.Name)
		}
	}
	add(ing.Spec.DefaultBackend)
	for _, rule := range ing.Spec.Rules {
		if rule.HTTP != nil {
			for i := range rule.HTTP.Paths {
				add(&rule.HTTP.Paths[i].Backend)
			}
		}
	}
	return out
}

// save writes st on every member (with the finalizer and the addresses once active), and lets go
// of an Ingress that left the group: its copy goes once the members hold the state, or once
// everything is gone (keeping the epoch, so a later group of the same name never replays a create
// key), and its finalizer with it unless another group now holds it. Until then a leaver keeps
// the state, even one that joined another group (which waits for it).
func (r *IngressReconciler) save(ctx context.Context, group string, members, holders []*networkingv1.Ingress, st State) error {
	data, _ := json.Marshal(st)
	for _, ing := range members {
		if err := r.patch(ctx, ing, string(data), true, &st); err != nil {
			return err
		}
	}
	for _, ing := range holders {
		if slices.Contains(members, ing) {
			continue
		}
		_, elsewhere := IngressGroup(ing)
		switch {
		case len(members) > 0:
			if err := r.patch(ctx, ing, "", elsewhere, nil); err != nil {
				return err
			}
		case st.Empty():
			kept := ""
			if st.Epoch > 0 {
				kept = string(data)
			}
			if err := r.patch(ctx, ing, kept, elsewhere, nil); err != nil {
				return err
			}
		default:
			if err := r.patch(ctx, ing, string(data), true, nil); err != nil {
				return err
			}
		}
	}
	return nil
}

// patch writes ing's state annotation and finalizer, and, for a member (status set), its addresses.
func (r *IngressReconciler) patch(ctx context.Context, ing *networkingv1.Ingress, state string, hold bool, status *State) error {
	orig := ing.DeepCopy()
	if ing.Annotations == nil {
		ing.Annotations = map[string]string{}
	}
	if state == "" {
		delete(ing.Annotations, lb.StateAnnotation)
	} else {
		ing.Annotations[lb.StateAnnotation] = state
	}
	if hold {
		controllerutil.AddFinalizer(ing, lb.Finalizer)
	} else {
		controllerutil.RemoveFinalizer(ing, lb.Finalizer)
	}
	if !equality.Semantic.DeepEqual(orig.ObjectMeta, ing.ObjectMeta) {
		if err := client.IgnoreNotFound(r.Client.Patch(ctx, ing, client.MergeFrom(orig))); err != nil {
			return err
		}
	}
	if ing.DeletionTimestamp != nil || status == nil {
		return nil
	}
	var addrs []networkingv1.IngressLoadBalancerIngress
	if status.Status == "active" {
		for _, ip := range []*string{status.IPv4, status.IPv6} {
			if ip != nil {
				addrs = append(addrs, networkingv1.IngressLoadBalancerIngress{IP: *ip})
			}
		}
	}
	if len(addrs) == 0 && len(ing.Status.LoadBalancer.Ingress) == 0 || equality.Semantic.DeepEqual(addrs, ing.Status.LoadBalancer.Ingress) {
		return nil
	}
	orig = ing.DeepCopy()
	ing.Status.LoadBalancer.Ingress = addrs
	return client.IgnoreNotFound(r.Client.Status().Patch(ctx, ing, client.MergeFrom(orig)))
}

// warn records an Ingress's refusals when they change.
func (r *IngressReconciler) warn(ing *networkingv1.Ingress, refused []lb.Refusal) {
	var msgs []string
	for _, f := range refused {
		msgs = append(msgs, f.Reason+": "+f.Message)
	}
	msg := strings.Join(msgs, "\n")
	if old, ok := r.warned.Swap(string(ing.UID), msg); ok && old.(string) == msg || !ok && msg == "" {
		return
	}
	for _, f := range refused {
		cloudevents.Warn(r.Recorder, ing, f.Reason, "%s", f.Message)
	}
}

// ingressFingerprint changes when any member, or a Service or Secret they name, does.
func ingressFingerprint(members []*networkingv1.Ingress, services map[string]*corev1.Service, secrets map[string]*corev1.Secret) string {
	h := sha256.New()
	for _, ing := range members {
		ann := map[string]string{}
		for k, v := range ing.Annotations {
			if strings.HasPrefix(k, lb.Prefix) && k != lb.StateAnnotation {
				ann[k] = v
			}
		}
		data, _ := json.Marshal(ann)
		fmt.Fprintf(h, "%s/%d/%s;", ing.UID, ing.Generation, data)
	}
	writeVersions(h, services, secrets)
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// IngressRequests maps an Ingress to the groups it's in and holds state for.
func IngressRequests(_ context.Context, obj client.Object) []reconcile.Request {
	ing, ok := obj.(*networkingv1.Ingress)
	if !ok {
		return nil
	}
	var out []reconcile.Request
	if g, ok := IngressGroup(ing); ok {
		out = append(out, groupRequest(g))
	} else if _, set := ing.Annotations[GroupAnnotation]; set && IsOurs(ing) && ing.DeletionTimestamp == nil {
		// An invalid group: reported by no group, so say it here once per change.
		out = append(out, groupRequest(ing.Namespace+"/"+ing.Name))
	}
	if st, err := readState(ing.Annotations); err == nil && st.Group != "" {
		out = append(out, groupRequest(st.Group))
	}
	return out
}

// IngressesNaming maps a Service or Secret to the groups of the ezgh Ingresses that name it.
func IngressesNaming(c client.Reader) func(context.Context, client.Object) []reconcile.Request {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		var list networkingv1.IngressList
		if err := c.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
			return nil
		}
		var out []reconcile.Request
		for i := range list.Items {
			ing := &list.Items[i]
			g, ok := IngressGroup(ing)
			if !ok {
				continue
			}
			named := false
			switch obj.(type) {
			case *corev1.Service:
				named = slices.Contains(ingressServices(ing), obj.GetName())
			case *corev1.Secret:
				named = slices.ContainsFunc(ing.Spec.TLS, func(t networkingv1.IngressTLS) bool { return t.SecretName == obj.GetName() })
			}
			if named {
				out = append(out, groupRequest(g))
			}
		}
		return out
	}
}

// IngressBackends are the target groups ezgh Ingress groups feed from svc, read from their state.
func IngressBackends(ctx context.Context, r client.Reader, svc *corev1.Service) ([]lb.Backend, error) {
	var list networkingv1.IngressList
	if err := r.List(ctx, &list); err != nil {
		return nil, err
	}
	var states []State
	for i := range list.Items {
		g, ok := IngressGroup(&list.Items[i])
		if !ok {
			continue
		}
		// Only its own group's state counts; lb-targets also checks each group carries it.
		if st, err := readState(list.Items[i].Annotations); err == nil && st.Group == g {
			states = append(states, st)
		}
	}
	return backendsOf(states, svc), nil
}

// StateServices maps an Ingress or Gateway to the Services its state feeds groups from.
func StateServices(_ context.Context, obj client.Object) []reconcile.Request {
	return stateServices(obj.GetAnnotations())
}

// backendsOf is every group in states fed from svc, once each.
func backendsOf(states []State, svc *corev1.Service) []lb.Backend {
	var out []lb.Backend
	for _, st := range states {
		for _, key := range slices.Sorted(maps.Keys(st.Groups)) {
			ns, name, port, ok := ParseBackendKey(key)
			if !ok || ns != svc.Namespace || name != svc.Name || slices.ContainsFunc(out, func(b lb.Backend) bool { return b.Group == st.Groups[key] }) {
				continue
			}
			for _, p := range svc.Spec.Ports {
				if int(p.Port) == port {
					out = append(out, lb.Backend{Group: st.Groups[key], Port: p.Name, Owner: st.Group})
				}
			}
		}
	}
	return out
}

// stateServices are the Services a state annotation's groups are fed from.
func stateServices(ann map[string]string) []reconcile.Request {
	st, err := readState(ann)
	if err != nil {
		return nil
	}
	var out []reconcile.Request
	for key := range st.Groups {
		if ns, name, _, ok := ParseBackendKey(key); ok {
			req := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}}
			if !slices.Contains(out, req) {
				out = append(out, req)
			}
		}
	}
	return out
}
