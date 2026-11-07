package l7

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"maps"
	"slices"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/cloud/config"
	cloudevents "github.com/ezgamehost/ezgh-cloud-controller/internal/cloud/events"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/cloud/lb"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/cloud/run"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient"
)

// Feeds are Ingress's and Gateway's sources for lb's targets controller and readiness gates.
func Feeds() []lb.Feed {
	return []lb.Feed{
		{Backends: IngressBackends, Object: &networkingv1.Ingress{}, Services: StateServices},
		{Backends: GatewayBackends, Object: &gatewayv1.Gateway{}, Services: StateServices},
	}
}

// Setup registers the Ingress, GatewayClass and Gateway controllers on env (leader only).
func Setup(env *run.Env) error {
	mgr, rec := env.Manager, env.Recorder("ezgh-lb")
	k := mgr.GetClient()
	opts := controller.Options{RateLimiter: run.RateLimiter()}
	ing := &IngressReconciler{Client: k, Clients: env.Clients, Config: env.Config, Recorder: rec}
	err := ctrl.NewControllerManagedBy(mgr).Named("l7-ingress").
		Watches(&networkingv1.Ingress{}, handler.EnqueueRequestsFromMapFunc(IngressRequests)).
		Watches(&corev1.Service{}, handler.EnqueueRequestsFromMapFunc(IngressesNaming(k)), builder.WithPredicates(lb.ServiceChanged)).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(IngressesNaming(k))).
		WithOptions(opts).Complete(ing)
	if err != nil {
		return err
	}
	err = ctrl.NewControllerManagedBy(mgr).Named("l7-gatewayclass").
		For(&gatewayv1.GatewayClass{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		WithOptions(opts).Complete(&GatewayClassReconciler{Client: k})
	if err != nil {
		return err
	}
	all := allGateways(k)
	gw := &GatewayReconciler{Client: k, Clients: env.Clients, Config: env.Config, Recorder: rec}
	return ctrl.NewControllerManagedBy(mgr).Named("l7-gateway").
		For(&gatewayv1.Gateway{}, builder.WithPredicates(predicate.Or(predicate.GenerationChangedPredicate{}, predicate.AnnotationChangedPredicate{}))).
		Watches(&gatewayv1.HTTPRoute{}, handler.EnqueueRequestsFromMapFunc(RouteGateways)).
		Watches(&gatewayv1.GatewayClass{}, handler.EnqueueRequestsFromMapFunc(all)).
		Watches(&gatewayv1.ReferenceGrant{}, handler.EnqueueRequestsFromMapFunc(all)).
		Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(all), builder.WithPredicates(predicate.LabelChangedPredicate{})).
		Watches(&corev1.Service{}, handler.EnqueueRequestsFromMapFunc(ServiceGateways(k)), builder.WithPredicates(lb.ServiceChanged)).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(SecretGateways(k))).
		WithOptions(opts).Complete(gw)
}

// allGateways maps any change to every Gateway.
// shortcut: a GatewayClass, ReferenceGrant or namespace-label change requeues every Gateway (each
// then reads what it names from the cache and writes nothing when nothing changed); they're rare.
func allGateways(c client.Reader) handler.MapFunc {
	return func(ctx context.Context, _ client.Object) []reconcile.Request {
		var list gatewayv1.GatewayList
		if err := c.List(ctx, &list); err != nil {
			return nil
		}
		out := make([]reconcile.Request, len(list.Items))
		for i, g := range list.Items {
			out[i] = reconcile.Request{NamespacedName: types.NamespacedName{Namespace: g.Namespace, Name: g.Name}}
		}
		return out
	}
}

// ServiceGateways maps a Service to the Gateways of the HTTPRoutes whose backendRefs name it.
func ServiceGateways(c client.Reader) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		var routes gatewayv1.HTTPRouteList
		if err := c.List(ctx, &routes); err != nil {
			return nil
		}
		var out []reconcile.Request
		for i := range routes.Items {
			rt := &routes.Items[i]
			named := slices.ContainsFunc(rt.Spec.Rules, func(rule gatewayv1.HTTPRouteRule) bool {
				return slices.ContainsFunc(rule.BackendRefs, func(br gatewayv1.HTTPBackendRef) bool {
					return string(br.Name) == obj.GetName() && string(ptr.Deref(br.Namespace, gatewayv1.Namespace(rt.Namespace))) == obj.GetNamespace()
				})
			})
			if named {
				for _, req := range RouteGateways(ctx, rt) {
					if !slices.Contains(out, req) {
						out = append(out, req)
					}
				}
			}
		}
		return out
	}
}

// SecretGateways maps a Secret to the Gateways whose listeners' certificateRefs name it.
func SecretGateways(c client.Reader) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		var list gatewayv1.GatewayList
		if err := c.List(ctx, &list); err != nil {
			return nil
		}
		var out []reconcile.Request
		for _, gw := range list.Items {
			named := slices.ContainsFunc(gw.Spec.Listeners, func(l gatewayv1.Listener) bool {
				return l.TLS != nil && slices.ContainsFunc(l.TLS.CertificateRefs, func(c gatewayv1.SecretObjectReference) bool {
					return string(c.Name) == obj.GetName() && string(ptr.Deref(c.Namespace, gatewayv1.Namespace(gw.Namespace))) == obj.GetNamespace()
				})
			})
			if named {
				out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: gw.Namespace, Name: gw.Name}})
			}
		}
		return out
	}
}

// RouteGateways maps an HTTPRoute to the Gateways it names, and those whose status it carries
// (so a detached route's entry goes).
func RouteGateways(_ context.Context, obj client.Object) []reconcile.Request {
	route, ok := obj.(*gatewayv1.HTTPRoute)
	if !ok {
		return nil
	}
	var out []reconcile.Request
	add := func(pr gatewayv1.ParentReference) {
		if ptr.Deref(pr.Group, GatewayGroup) != GatewayGroup || ptr.Deref(pr.Kind, "Gateway") != "Gateway" {
			return
		}
		req := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: string(ptr.Deref(pr.Namespace, gatewayv1.Namespace(route.Namespace))), Name: string(pr.Name)}}
		if !slices.Contains(out, req) {
			out = append(out, req)
		}
	}
	for _, pr := range route.Spec.ParentRefs {
		add(pr)
	}
	for _, p := range route.Status.Parents {
		if p.ControllerName == ControllerName {
			add(p.ParentRef)
		}
	}
	return out
}

// GatewayBackends are the target groups ezgh Gateways feed from svc, read from their state.
func GatewayBackends(ctx context.Context, r client.Reader, svc *corev1.Service) ([]lb.Backend, error) {
	var list gatewayv1.GatewayList
	if err := r.List(ctx, &list); err != nil {
		return nil, err
	}
	var states []State
	for _, g := range list.Items {
		if g.DeletionTimestamp != nil {
			continue
		}
		// Only its own state counts; lb-targets also checks each group carries it.
		if st, err := readState(g.Annotations); err == nil && st.Group == g.Namespace+"/"+g.Name {
			states = append(states, st)
		}
	}
	return backendsOf(states, svc), nil
}

// GatewayClassReconciler accepts every GatewayClass whose controller is ours.
type GatewayClassReconciler struct{ Client client.Client }

// Reconcile sets Accepted and the supported features.
func (r *GatewayClassReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	var gc gatewayv1.GatewayClass
	if err := r.Client.Get(ctx, req.NamespacedName, &gc); err != nil || gc.Spec.ControllerName != ControllerName {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	orig := gc.DeepCopy()
	meta.SetStatusCondition(&gc.Status.Conditions, condition("Accepted", true, "Accepted", "", gc.Generation))
	gc.Status.SupportedFeatures = SupportedFeatures
	if equality.Semantic.DeepEqual(orig.Status, gc.Status) {
		return reconcile.Result{}, nil
	}
	return reconcile.Result{}, r.Client.Status().Patch(ctx, &gc, client.MergeFrom(orig))
}

// GatewayReconciler keeps one load balancer per ezgh Gateway, and its and its routes' status.
type GatewayReconciler struct {
	Client   client.Client
	Clients  *ezghclient.Clients
	Config   func() (config.Config, error)
	Recorder events.EventRecorder
	warned   sync.Map // Gateway UID → last refusals reported
}

// Reconcile converges one Gateway. Its state is in its lb.ezghcloud.com/state annotation, and
// the finalizer holds it until its load balancer, groups, pip_ and imported certificates are gone.
func (r *GatewayReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	var gw gatewayv1.Gateway
	if err := r.Client.Get(ctx, req.NamespacedName, &gw); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	var gc gatewayv1.GatewayClass
	if err := r.Client.Get(ctx, types.NamespacedName{Name: string(gw.Spec.GatewayClassName)}, &gc); apierrors.IsNotFound(err) {
		if gw.DeletionTimestamp == nil {
			// Nothing is deleted because something is missing: without its class a Gateway keeps
			// what serves (and a new one isn't programmed) until the class is back or it's deleted.
			return reconcile.Result{}, nil
		}
	} else if err != nil {
		return reconcile.Result{}, err
	}
	live := gc.Spec.ControllerName == ControllerName && gw.DeletionTimestamp == nil
	if !live && !controllerutil.ContainsFinalizer(&gw, lb.Finalizer) {
		return reconcile.Result{}, nil
	}
	cfg, err := r.Config()
	if err != nil {
		return reconcile.Result{}, err
	}
	have, err := readState(gw.Annotations)
	if err != nil {
		cloudevents.Warn(r.Recorder, &gw, cloudevents.InvalidAnnotation, "%v", err)
		return reconcile.Result{}, err
	}
	ref := gw.Namespace + "/" + gw.Name
	want := Model{Tags: lb.Tags(cfg, ref), Named: GatewayLBName(&gw)}
	var status Status
	var routes []gatewayv1.HTTPRoute
	if live {
		var in GatewayInput
		if in, routes, err = r.inputs(ctx, &gw); err != nil {
			return reconcile.Result{}, err
		}
		want, status = FromGateway(&gw, in, cfg)
		r.warn(&gw, status.Refused)
		if want.Name == "" {
			// Not accepted (spec.addresses, no valid listener): what exists keeps serving, as a typo
			// shouldn't change it.
			return reconcile.Result{}, r.status(ctx, &gw, status, routes, have, false, "The Gateway isn't accepted")
		}
		if !controllerutil.ContainsFinalizer(&gw, lb.Finalizer) {
			orig := gw.DeepCopy()
			controllerutil.AddFinalizer(&gw, lb.Finalizer)
			if err := r.Client.Patch(ctx, &gw, client.MergeFrom(orig)); err != nil {
				return reconcile.Result{}, err
			}
		}
		if have.Failed != "" && have.Failed == gatewayFingerprint(&gw, in) {
			return reconcile.Result{}, r.status(ctx, &gw, status, routes, have, false, "The load balancer's create failed; change the Gateway to try again")
		}
		have.Failed = ""
	} else {
		var list gatewayv1.HTTPRouteList
		if err := r.Client.List(ctx, &list); err != nil {
			return reconcile.Result{}, err
		}
		routes = list.Items
	}
	st, applyErr := Apply(ctx, r.Clients, cfg, have, want)
	st.Group, st.Key = ref, want.Key
	var failed *lb.CreateFailedError
	if errors.As(applyErr, &failed) && live {
		in, _, _ := r.inputs(ctx, &gw)
		st.Failed = gatewayFingerprint(&gw, in)
	}
	if err := r.save(ctx, &gw, st, live); err != nil {
		return reconcile.Result{}, err
	}
	lb.WarnForged(r.Recorder, &gw, st.Forged)
	why := "Waiting for the load balancer to be active"
	if _, busy := run.Busy(applyErr); busy {
		why = "Waiting for the load balancer's current operation to finish"
	} else if applyErr != nil {
		_, why = cloudevents.Describe(applyErr)
	}
	if err := r.status(ctx, &gw, status, routes, st, st.Status == "active" && applyErr == nil, why); err != nil && applyErr == nil {
		return reconcile.Result{}, err
	}
	if failed != nil {
		cloudevents.Warn(r.Recorder, &gw, cloudevents.EZGHAPIError, "%s. Change the Gateway to try again.", failed.Error())
		return reconcile.Result{}, nil
	}
	if applyErr != nil {
		return run.Fail(r.Recorder, &gw, applyErr) // Review Focus 1: an event, backoff, nothing deleted
	}
	if live && st.Status != "active" || !live && !st.Empty() {
		// Provisioning, or tearing down: read again until it's active or gone.
		return reconcile.Result{RequeueAfter: provisioningPoll}, nil
	}
	return reconcile.Result{}, nil
}

// inputs reads what gw's model needs from the cache, and every HTTPRoute.
func (r *GatewayReconciler) inputs(ctx context.Context, gw *gatewayv1.Gateway) (GatewayInput, []gatewayv1.HTTPRoute, error) {
	in := GatewayInput{Services: map[string]*corev1.Service{}, Secrets: map[string]*corev1.Secret{}, Namespaces: map[string]map[string]string{}}
	var routes gatewayv1.HTTPRouteList
	var grants gatewayv1.ReferenceGrantList
	var nss corev1.NamespaceList
	if err := r.Client.List(ctx, &routes); err != nil {
		return in, nil, err
	}
	if err := r.Client.List(ctx, &grants); err != nil {
		return in, nil, err
	}
	if err := r.Client.List(ctx, &nss); err != nil {
		return in, nil, err
	}
	in.Grants = grants.Items
	for _, ns := range nss.Items {
		in.Namespaces[ns.Name] = ns.Labels
	}
	for _, route := range routes.Items {
		if !slices.ContainsFunc(route.Spec.ParentRefs, func(pr gatewayv1.ParentReference) bool { return refersTo(pr, route.Namespace, gw) }) {
			continue
		}
		in.Routes = append(in.Routes, route)
		for _, rule := range route.Spec.Rules {
			for _, br := range rule.BackendRefs {
				ns := string(ptr.Deref(br.Namespace, gatewayv1.Namespace(route.Namespace)))
				var svc corev1.Service
				if err := getIfExists(ctx, r.Client, ns, string(br.Name), &svc); err != nil {
					return in, nil, err
				} else if svc.Name != "" {
					in.Services[ns+"/"+svc.Name] = &svc
				}
			}
		}
	}
	for _, l := range gw.Spec.Listeners {
		if l.TLS == nil {
			continue
		}
		for _, c := range l.TLS.CertificateRefs {
			ns := string(ptr.Deref(c.Namespace, gatewayv1.Namespace(gw.Namespace)))
			var s corev1.Secret
			if err := getIfExists(ctx, r.Client, ns, string(c.Name), &s); err != nil {
				return in, nil, err
			} else if s.Name != "" {
				in.Secrets[ns+"/"+s.Name] = &s
			}
		}
	}
	return in, routes.Items, nil
}

// save records st on gw and holds or releases the finalizer.
func (r *GatewayReconciler) save(ctx context.Context, gw *gatewayv1.Gateway, st State, live bool) error {
	orig := gw.DeepCopy()
	data, _ := json.Marshal(st)
	if gw.Annotations == nil {
		gw.Annotations = map[string]string{}
	}
	gw.Annotations[lb.StateAnnotation] = string(data)
	if st.Empty() && st.Epoch == 0 {
		delete(gw.Annotations, lb.StateAnnotation)
	}
	if live {
		controllerutil.AddFinalizer(gw, lb.Finalizer)
	} else if st.Empty() {
		controllerutil.RemoveFinalizer(gw, lb.Finalizer)
	}
	if equality.Semantic.DeepEqual(orig.ObjectMeta, gw.ObjectMeta) {
		return nil
	}
	return client.IgnoreNotFound(r.Client.Patch(ctx, gw, client.MergeFrom(orig)))
}

// status writes the Gateway's status (a live one's) and this Gateway's entries in every route's.
func (r *GatewayReconciler) status(ctx context.Context, gw *gatewayv1.Gateway, status Status, routes []gatewayv1.HTTPRoute, st State, programmed bool, why string) error {
	if gw.DeletionTimestamp != nil {
		// Deletion bumps the generation by one and changes nothing the conditions judged: those
		// of the generation before are restamped while the load balancer is torn down.
		orig := gw.DeepCopy()
		restamp(gw.Status.Conditions, gw.Generation)
		for i := range gw.Status.Listeners {
			restamp(gw.Status.Listeners[i].Conditions, gw.Generation)
		}
		if !equality.Semantic.DeepEqual(orig.Status, gw.Status) {
			if err := r.Client.Status().Patch(ctx, gw, client.MergeFrom(orig)); client.IgnoreNotFound(err) != nil {
				return err
			}
		}
	} else if len(status.Gateway) > 0 {
		orig := gw.DeepCopy()
		var addrs []string
		for _, ip := range []*string{st.IPv4, st.IPv6} {
			if ip != nil {
				addrs = append(addrs, *ip)
			}
		}
		SetGatewayStatus(gw, status, programmed, why, addrs)
		if !equality.Semantic.DeepEqual(orig.Status, gw.Status) {
			if err := r.Client.Status().Patch(ctx, gw, client.MergeFrom(orig)); err != nil {
				return err
			}
		}
	}
	for i := range routes {
		route := &routes[i]
		orig := route.DeepCopy()
		SetRouteParents(route, gw, status.Routes[types.NamespacedName{Namespace: route.Namespace, Name: route.Name}])
		if equality.Semantic.DeepEqual(orig.Status, route.Status) {
			continue
		}
		// Optimistic: other controllers write other parents' entries of the same list.
		if err := r.Client.Status().Patch(ctx, route, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{})); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return nil
}

func restamp(conds []metav1.Condition, gen int64) {
	for i := range conds {
		if conds[i].ObservedGeneration == gen-1 {
			conds[i].ObservedGeneration = gen
		}
	}
}

// warn records a Gateway's refusals when they change.
func (r *GatewayReconciler) warn(gw *gatewayv1.Gateway, refused []lb.Refusal) {
	var msgs []string
	for _, f := range refused {
		msgs = append(msgs, f.Reason+": "+f.Message)
	}
	msg := strings.Join(msgs, "\n")
	if old, ok := r.warned.Swap(string(gw.UID), msg); ok && old.(string) == msg || !ok && msg == "" {
		return
	}
	for _, f := range refused {
		cloudevents.Warn(r.Recorder, gw, f.Reason, "%s", f.Message)
	}
}

// gatewayFingerprint changes when the Gateway, a route of it, or what they name does.
func gatewayFingerprint(gw *gatewayv1.Gateway, in GatewayInput) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s/%d;", gw.UID, gw.Generation)
	for _, rt := range in.Routes {
		fmt.Fprintf(h, "%s/%d;", rt.UID, rt.Generation)
	}
	writeVersions(h, in.Services, in.Secrets)
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// writeVersions hashes the resource versions of the Services and Secrets a model read.
func writeVersions(h hash.Hash, services map[string]*corev1.Service, secrets map[string]*corev1.Secret) {
	for _, k := range slices.Sorted(maps.Keys(services)) {
		fmt.Fprintf(h, "%s/%s;", k, services[k].ResourceVersion)
	}
	for _, k := range slices.Sorted(maps.Keys(secrets)) {
		fmt.Fprintf(h, "%s/%s;", k, secrets[k].ResourceVersion)
	}
}
