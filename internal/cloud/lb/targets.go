package lb

import (
	"cmp"
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/cloud/config"
	cloudevents "github.com/ezgamehost/ezgh-cloud-controller/internal/cloud/events"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/cloud/run"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/apiclient"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/lbapi"
)

// gatePoll is how often a Service's targets are read while one of its targeted pods' gates is
// unset; drainWait is how long a group at its target limit is left before trying again.
const (
	gatePoll  = 5 * time.Second
	drainWait = 30 * time.Second
)

// Target is a pod address and port in a target group.
type Target struct {
	Address string
	Port    int
}

// Backend is a target group fed from one of a Service's ports.
type Backend struct {
	Group string // tgp_
	Port  string // the Service port's name, as EndpointSlices name it ("" when unnamed)
	// Owner is the ezgh:kubernetes-service tag the group must carry (it came from a state
	// annotation, which anyone editing the object can write).
	Owner string
}

// BackendSource names the target groups one kind of load-balancing object feeds from svc.
type BackendSource func(ctx context.Context, r client.Reader, svc *corev1.Service) ([]Backend, error)

// Backends are every source (L4 Services here; L7 adds Ingress and Gateway): the targets
// controller, the gates and the webhook ask them all.
type Backends []BackendSource

// For is every target group fed from svc.
func (b Backends) For(ctx context.Context, r client.Reader, svc *corev1.Service) ([]Backend, error) {
	var out []Backend
	for _, src := range b {
		got, err := src(ctx, r, svc)
		if err != nil {
			return nil, err
		}
		out = append(out, got...)
	}
	return out, nil
}

// ServiceBackends are an L4 Service's own groups, from its state annotation.
func ServiceBackends(_ context.Context, _ client.Reader, svc *corev1.Service) ([]Backend, error) {
	if !Handled(svc) || svc.DeletionTimestamp != nil {
		return nil, nil
	}
	st, err := ReadState(svc.Annotations)
	if err != nil {
		return nil, err
	}
	if st.LoadBalancer == "" { // a failed create leaves groups but nothing to gate on
		return nil, nil
	}
	var out []Backend
	for _, p := range svc.Spec.Ports {
		if id := st.Groups[GroupKey(svc, p)]; id != "" {
			out = append(out, Backend{Group: id, Port: p.Name, Owner: svc.Namespace + "/" + svc.Name})
		}
	}
	return out, nil
}

// Targets are the IPv4 endpoints of slices on the port named portName, sorted, at most MaxTargets,
// and how many more there were.
func Targets(eps []discoveryv1.EndpointSlice, portName string, pods map[string]*corev1.Pod) (want []Target, overflow int) {
	want = eligible(eps, portName, pods)
	if len(want) > MaxTargets {
		return want[:MaxTargets], len(want) - MaxTargets
	}
	return want, 0
}

// eligible is every IPv4 endpoint that should be a target, sorted. An endpoint is a target when it's ready, or when only readiness
// gates of ours hold its pod back (else a gated pod could never become healthy). Terminating
// endpoints are left out, so they're deregistered at once and drain for the group's delay.
func eligible(eps []discoveryv1.EndpointSlice, portName string, pods map[string]*corev1.Pod) (want []Target) {
	seen := map[Target]bool{}
	for _, s := range eps {
		if s.AddressType != discoveryv1.AddressTypeIPv4 {
			continue
		}
		port := 0
		for _, p := range s.Ports {
			if ptrString(p.Name) == portName && p.Port != nil {
				port = int(*p.Port)
			}
		}
		if port == 0 {
			continue
		}
		for _, e := range s.Endpoints {
			if len(e.Addresses) == 0 || e.Conditions.Terminating != nil && *e.Conditions.Terminating {
				continue
			}
			if e.Conditions.Ready != nil && !*e.Conditions.Ready && !onlyOurGates(e.TargetRef, pods) {
				continue
			}
			seen[Target{Address: e.Addresses[0], Port: port}] = true
		}
	}
	for t := range seen {
		want = append(want, t)
	}
	slices.SortFunc(want, func(a, b Target) int {
		x, _ := netip.ParseAddr(a.Address)
		y, _ := netip.ParseAddr(b.Address)
		return cmp.Or(x.Compare(y), cmp.Compare(a.Port, b.Port))
	})
	return want
}

// onlyOurGates reports whether the endpoint's pod is running with its containers ready and every
// readiness gate but ours True.
func onlyOurGates(ref *corev1.ObjectReference, pods map[string]*corev1.Pod) bool {
	if ref == nil || ref.Kind != "Pod" || pods[ref.Name] == nil {
		return false
	}
	pod := pods[ref.Name]
	if pod.DeletionTimestamp != nil || conditionStatus(pod, corev1.ContainersReady) != corev1.ConditionTrue {
		return false
	}
	for _, g := range pod.Spec.ReadinessGates {
		if !strings.HasPrefix(string(g.ConditionType), GatePrefix) && conditionStatus(pod, g.ConditionType) != corev1.ConditionTrue {
			return false
		}
	}
	return true
}

// SyncTargets makes group's targets want: it lists them, registers the missing ones (one call
// each, privateIpv4 and port: D13), and deregisters the rest. It returns the group's targets as
// they are now, for the gates. Draining targets still count toward lb-api's 500 per group, so a
// rollout at the cap can get 409 target_limit until they drain.
func SyncTargets(ctx context.Context, c *ezghclient.Clients, cfg config.Config, group string, want []Target) ([]lbapi.Target, error) {
	have, err := all(func(cursor string) (apiclient.Page[lbapi.Target], error) {
		return c.LB.ListTargets(ctx, cfg.Org, cfg.Project, group, pageSize, cursor)
	})
	if err != nil {
		return nil, err
	}
	wanted := map[Target]bool{}
	for _, t := range want {
		wanted[t] = true
	}
	var out []lbapi.Target
	present := map[Target]bool{}
	for _, t := range have {
		k := Target{Address: t.PrivateIPv4, Port: ptrInt(t.Port)}
		if wanted[k] && t.State != "draining" {
			present[k] = true
			out = append(out, t)
			continue
		}
		if t.State == "draining" {
			continue
		}
		if _, err := c.LB.DeregisterTarget(ctx, cfg.Org, cfg.Project, group, t.ID); err != nil {
			if err := deleted(ctx, c, cfg, err); err != nil {
				return out, err
			}
		}
	}
	for _, t := range want {
		if present[t] {
			continue
		}
		made, err := c.LB.RegisterTarget(ctx, cfg.Org, cfg.Project, group, lbapi.TargetCreate{PrivateIPv4: &t.Address, Port: &t.Port})
		if ezghclient.Code(err) == "target_already_registered" {
			continue
		}
		if err != nil {
			return out, err
		}
		out = append(out, *made)
	}
	return out, nil
}

// TargetsReconciler keeps the targets of every group fed from a Service, and its pods' gates.
type TargetsReconciler struct {
	Client   client.Client
	Clients  *ezghclient.Clients
	Config   func() (config.Config, error)
	Recorder events.EventRecorder
	Backends Backends
	noted    notes
	// verified holds groups found to carry the cluster's tag and their owner's. IDs aren't reused
	// and only the cluster's Bot writes ezgh:kubernetes-* tags (D2), so a group is read once.
	verified sync.Map // tgp_ → owner
}

// ours reports whether b's group is the cluster's and b's owner's, reading it the first time.
func (r *TargetsReconciler) ours(ctx context.Context, cfg config.Config, b Backend) (bool, error) {
	if v, ok := r.verified.Load(b.Group); ok && v == b.Owner {
		return true, nil
	}
	tg, err := r.Clients.LB.GetTargetGroup(ctx, cfg.Org, cfg.Project, b.Group)
	if err != nil {
		if ezghclient.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	want := Tags(cfg, b.Owner)
	for k, v := range want {
		if tg.Tags[k] != v {
			return false, nil
		}
	}
	r.verified.Store(b.Group, b.Owner)
	return true, nil
}

// Reconcile syncs one Service's targets, then its pods' gates.
func (r *TargetsReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	var svc corev1.Service
	var backends []Backend
	err := r.Client.Get(ctx, req.NamespacedName, &svc)
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return reconcile.Result{}, err
	default:
		if backends, err = r.Backends.For(ctx, r.Client, &svc); err != nil {
			return reconcile.Result{}, err
		}
	}
	limitNote, ipv6Note := req.String()+"/limit", req.String()+"/ipv6"
	if len(backends) == 0 {
		r.noted.Delete(limitNote)
		r.noted.Delete(ipv6Note)
	}
	pods, err := r.pods(ctx, req.Namespace)
	if err != nil {
		return reconcile.Result{}, err
	}
	health := map[string]map[string]string{}
	capped := map[string]bool{}
	var res reconcile.Result
	if len(backends) > 0 {
		cfg, err := r.Config()
		if err != nil {
			return reconcile.Result{}, err
		}
		var sl discoveryv1.EndpointSliceList
		if err := r.Client.List(ctx, &sl, client.InNamespace(req.Namespace), client.MatchingLabels{discoveryv1.LabelServiceName: req.Name}); err != nil {
			return reconcile.Result{}, err
		}
		byName := map[string]*corev1.Pod{}
		for i := range pods {
			byName[pods[i].Name] = &pods[i]
		}
		overflow, limited := 0, false
		// A group that isn't the cluster's and its owner's (a forged state) is left alone, and
		// doesn't hold gates either.
		var checked []Backend
		for _, b := range backends {
			ok, err := r.ours(ctx, cfg, b)
			if err != nil {
				return run.Fail(r.Recorder, &svc, err)
			}
			if ok {
				checked = append(checked, b)
			} else if r.noted.changed(req.String()+"/forged/"+b.Group, "forged") {
				WarnForged(r.Recorder, &svc, []string{"target group " + b.Group + " (fed from this Service)"})
			}
		}
		backends = checked
		for _, b := range backends {
			ready := eligible(sl.Items, b.Port, byName)
			want := ready[:min(len(ready), MaxTargets)]
			for _, t := range ready[len(want):] {
				capped[t.Address] = true
			}
			overflow = max(overflow, len(ready)-len(want))
			got, err := SyncTargets(ctx, r.Clients, cfg, b.Group, want)
			if ezghclient.Code(err) == "target_limit" {
				limited, res.RequeueAfter = true, drainWait
			} else if err != nil {
				return run.Fail(r.Recorder, &svc, err)
			}
			health[b.Group] = map[string]string{}
			for _, t := range got {
				health[b.Group][t.PrivateIPv4] = t.State
			}
		}
		msg := ""
		if overflow > 0 {
			msg = fmt.Sprintf("%d endpoints aren't load-balanced: a target group holds at most %d targets", overflow, MaxTargets)
		} else if limited {
			msg = fmt.Sprintf("A target group is at its %d targets (draining ones count until they go); the rest register once they drain", MaxTargets)
		}
		if r.noted.changed(limitNote, msg) {
			cloudevents.Warn(r.Recorder, &svc, cloudevents.TargetLimit, "%s", msg)
		}
		msg = ""
		if !slices.Contains(svc.Spec.IPFamilies, corev1.IPv4Protocol) {
			msg = "EZGH load balancers take IPv4 targets only; this Service's endpoints are IPv6 targets, so none are registered"
		}
		if r.noted.changed(ipv6Note, msg) {
			cloudevents.Warn(r.Recorder, &svc, cloudevents.UnsupportedProtocol, "%s", msg)
		}
	}
	pending, err := r.gates(ctx, req.Name, pods, backends, health, capped)
	if err != nil {
		return reconcile.Result{}, err
	}
	if pending && (res.RequeueAfter == 0 || gatePoll < res.RequeueAfter) {
		res.RequeueAfter = gatePoll
	}
	return res, nil
}

func (r *TargetsReconciler) pods(ctx context.Context, ns string) ([]corev1.Pod, error) {
	var pl corev1.PodList
	err := r.Client.List(ctx, &pl, client.InNamespace(ns))
	return pl.Items, err
}

// SliceService maps an EndpointSlice to its Service.
func SliceService(_ context.Context, obj client.Object) []reconcile.Request {
	name := obj.GetLabels()[discoveryv1.LabelServiceName]
	if name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: name}}}
}

// PodGateServices maps a pod to the Services whose gates it carries unset.
func PodGateServices(_ context.Context, obj client.Object) []reconcile.Request {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return nil
	}
	var out []reconcile.Request
	for _, g := range pod.Spec.ReadinessGates {
		if name, ok := strings.CutPrefix(string(g.ConditionType), GatePrefix); ok && conditionStatus(pod, g.ConditionType) != corev1.ConditionTrue {
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: pod.Namespace, Name: name}})
		}
	}
	return out
}

func ptrString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func ptrInt(n *int) int {
	if n == nil {
		return 0
	}
	return *n
}
