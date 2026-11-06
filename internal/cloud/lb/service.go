package lb

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/runtime"
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
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/cloud/config"
	cloudevents "github.com/ezgamehost/ezgh-cloud-controller/internal/cloud/events"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/cloud/run"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient"
)

// Finalizer holds a Service until its load balancer, target groups and owned pip_ are gone.
const Finalizer = Prefix + "resources"

// provisioningPoll is how often a load balancer that isn't active yet is read again (an API read,
// not a table poll).
const provisioningPoll = 15 * time.Second

// Feed is another kind of object whose load balancers take targets from Services (L7's Ingress
// and Gateway): the groups it feeds from a Service, and the Services a change to one touches.
type Feed struct {
	Backends BackendSource
	Object   client.Object
	Services handler.MapFunc
}

// Setup registers the Service and targets controllers and the readiness-gate webhook on env.
// The webhook runs on every control-plane VM; the controllers only on the leader.
func Setup(env *run.Env, feeds ...Feed) error {
	mgr, rec := env.Manager, env.Recorder("ezgh-lb")
	backends := Backends{ServiceBackends}
	for _, f := range feeds {
		backends = append(backends, f.Backends)
	}
	svc := &ServiceReconciler{Client: mgr.GetClient(), Clients: env.Clients, Config: env.Config, Recorder: rec}
	err := ctrl.NewControllerManagedBy(mgr).Named("lb-service").
		For(&corev1.Service{}, builder.WithPredicates(predicate.Or(predicate.GenerationChangedPredicate{}, predicate.AnnotationChangedPredicate{}))).
		WithOptions(controller.Options{RateLimiter: run.RateLimiter()}).
		Complete(svc)
	if err != nil {
		return err
	}
	targets := &TargetsReconciler{Client: mgr.GetClient(), Clients: env.Clients, Config: env.Config, Recorder: rec, Backends: backends}
	b := ctrl.NewControllerManagedBy(mgr).Named("lb-targets").
		For(&corev1.Service{}).
		Watches(&discoveryv1.EndpointSlice{}, handler.EnqueueRequestsFromMapFunc(SliceService)).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(PodGateServices))
	for _, f := range feeds {
		b = b.Watches(f.Object, handler.EnqueueRequestsFromMapFunc(f.Services))
	}
	err = b.WithOptions(controller.Options{RateLimiter: run.RateLimiter()}).Complete(targets)
	if err != nil {
		return err
	}
	mgr.GetWebhookServer().Register(WebhookPath, &webhook.Admission{Handler: &Webhook{Client: mgr.GetClient(), Backends: backends}})
	return nil
}

// ServiceReconciler keeps one load balancer per LoadBalancer Service.
type ServiceReconciler struct {
	Client   client.Client
	Clients  *ezghclient.Clients
	Config   func() (config.Config, error)
	Recorder events.EventRecorder
	warned   notes
}

// Reconcile converges one Service.
func (r *ServiceReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	var svc corev1.Service
	if err := r.Client.Get(ctx, req.NamespacedName, &svc); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	live := Handled(&svc) && svc.DeletionTimestamp == nil
	if !live && !controllerutil.ContainsFinalizer(&svc, Finalizer) {
		return reconcile.Result{}, nil
	}
	cfg, err := r.Config()
	if err != nil {
		return reconcile.Result{}, err
	}
	have, err := ReadState(svc.Annotations)
	if err != nil {
		cloudevents.Warn(r.Recorder, &svc, cloudevents.InvalidAnnotation, "%v", err)
		return reconcile.Result{}, err
	}
	want := Model{Tags: Tags(cfg, svc.Namespace+"/"+svc.Name), Named: ServiceLBName(&svc)}
	if live {
		var refused []Refusal
		want, refused = Desired(&svc, cfg)
		if have.LoadBalancer != "" && want.IPv4Address != "" && want.IPv4Address != have.IPv4Address {
			refused = append(refused, Refusal{cloudevents.InvalidAnnotation, Prefix + "ipv4-address can't change on an existing load balancer: it keeps " + have.IPv4Address})
		}
		r.warn(&svc, refused)
		if want.Name == "" {
			// Nothing servable (or a typo): what exists keeps serving.
			return reconcile.Result{}, nil
		}
		if have.Failed != "" && have.Failed == fingerprint(&svc) {
			return reconcile.Result{}, nil // its create failed; reported, and retried when the Service changes
		}
		have.Failed = ""
		// The finalizer goes on before anything is made, so a Service deleted meanwhile can't leak it.
		if !controllerutil.ContainsFinalizer(&svc, Finalizer) {
			orig := svc.DeepCopy()
			controllerutil.AddFinalizer(&svc, Finalizer)
			if err := r.Client.Patch(ctx, &svc, client.MergeFrom(orig)); err != nil {
				return reconcile.Result{}, err
			}
		}
	}

	st, applyErr := Apply(ctx, r.Clients, cfg, have, want)
	WarnForged(r.Recorder, &svc, st.Forged)
	var failed *CreateFailedError
	if errors.As(applyErr, &failed) {
		st.Failed = fingerprint(&svc)
	}
	if err := r.save(ctx, &svc, st, live); err != nil {
		return reconcile.Result{}, err
	}
	if failed != nil {
		cloudevents.Warn(r.Recorder, &svc, cloudevents.EZGHAPIError, "%s. Change the Service to try again.", failed.Error())
		return reconcile.Result{}, nil
	}
	if applyErr != nil {
		return run.Fail(r.Recorder, &svc, applyErr)
	}
	if !live && st.Empty() {
		r.warned.Delete(string(svc.UID))
		return reconcile.Result{}, nil
	}
	if st.Status != "active" {
		return reconcile.Result{RequeueAfter: provisioningPoll}, nil
	}
	return reconcile.Result{}, nil
}

// save records st in the annotation, holds or releases the finalizer, and sets the status.
func (r *ServiceReconciler) save(ctx context.Context, svc *corev1.Service, st State, live bool) error {
	orig := svc.DeepCopy()
	data, _ := json.Marshal(st)
	if svc.Annotations == nil {
		svc.Annotations = map[string]string{}
	}
	svc.Annotations[StateAnnotation] = string(data)
	if st.Empty() && st.Epoch == 0 {
		delete(svc.Annotations, StateAnnotation)
	}
	if live {
		controllerutil.AddFinalizer(svc, Finalizer)
	} else if st.Empty() {
		controllerutil.RemoveFinalizer(svc, Finalizer)
	}
	if !equality.Semantic.DeepEqual(orig.ObjectMeta, svc.ObjectMeta) {
		if err := r.Client.Patch(ctx, svc, client.MergeFrom(orig)); err != nil {
			return err
		}
	}
	if svc.DeletionTimestamp != nil {
		return nil
	}
	var ingress []corev1.LoadBalancerIngress
	if live && st.Status == "active" {
		// Proxy, so kube-proxy sends in-cluster clients through the load balancer too (proxy
		// protocol and TLS happen there).
		for _, ip := range []*string{st.IPv4, st.IPv6} {
			if ip != nil {
				ingress = append(ingress, corev1.LoadBalancerIngress{IP: *ip, IPMode: ptr.To(corev1.LoadBalancerIPModeProxy)})
			}
		}
	}
	if equality.Semantic.DeepEqual(ingress, svc.Status.LoadBalancer.Ingress) || len(ingress) == 0 && len(svc.Status.LoadBalancer.Ingress) == 0 {
		return nil
	}
	orig = svc.DeepCopy()
	svc.Status.LoadBalancer.Ingress = ingress
	return r.Client.Status().Patch(ctx, svc, client.MergeFrom(orig))
}

// WarnForged reports what a state annotation named that isn't its object's, and was forgotten.
func WarnForged(rec events.EventRecorder, obj runtime.Object, forged []string) {
	for _, f := range forged {
		cloudevents.Warn(rec, obj, cloudevents.InvalidAnnotation, "%s named %s, which isn't this object's (its tags or name differ): forgotten, never changed", StateAnnotation, f)
	}
}

// warn records refusals when they change, so a Service requeued every 15 s isn't flooded.
func (r *ServiceReconciler) warn(svc *corev1.Service, refused []Refusal) {
	var msgs []string
	for _, f := range refused {
		msgs = append(msgs, f.Reason+": "+f.Message)
	}
	if !r.warned.changed(string(svc.UID), strings.Join(msgs, "\n")) {
		return
	}
	for _, f := range refused {
		cloudevents.Warn(r.Recorder, svc, f.Reason, "%s", f.Message)
	}
}

// fingerprint changes when the Service's spec or any of its lb.ezghcloud.com/ annotations do.
func fingerprint(svc *corev1.Service) string {
	ann := map[string]string{}
	for k, v := range svc.Annotations {
		if strings.HasPrefix(k, Prefix) && k != StateAnnotation {
			ann[k] = v
		}
	}
	data, _ := json.Marshal(ann) // map keys are sorted
	sum := sha256.Sum256(append(strconv.AppendInt(nil, svc.Generation, 10), data...))
	return hex.EncodeToString(sum[:8])
}

// notes remembers the last message per key, to report a condition once per change. Leader
// failover reports it once more.
type notes struct{ m sync.Map }

func (n *notes) changed(key, msg string) bool {
	if msg == "" {
		n.m.Delete(key)
		return false
	}
	old, ok := n.m.Swap(key, msg)
	return !ok || old.(string) != msg
}

func (n *notes) Delete(key string) { n.m.Delete(key) }
