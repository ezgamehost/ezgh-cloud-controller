package lb

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/cloud/config"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/lbapi"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/networkapi"
)

var ctx = context.Background()

func twoPorts() *corev1.Service {
	return service([]corev1.ServicePort{tcpPort("http", 80, intstr.FromInt32(8080)), tcpPort("admin", 9000, intstr.FromInt32(9000))}, nil)
}

func model(t *testing.T, svc *corev1.Service) Model {
	t.Helper()
	m, refused := Desired(svc, testCfg)
	if len(refused) != 0 {
		t.Fatal(refused)
	}
	return m
}

// converge applies m until its load balancer is active and its listeners made.
func converge(t *testing.T, c *ezghclient.Clients, st State, m Model) State {
	t.Helper()
	for range 3 {
		var err error
		if st, err = Apply(ctx, c, testCfg, st, m); err != nil {
			t.Fatal(err)
		}
		if st.Status == "active" && len(st.Listeners) == len(m.Listeners) {
			return st
		}
	}
	t.Fatalf("not converged: %+v", st)
	return st
}

func hasPrefixes(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("writes %q, want %q", got, want)
	}
	for i := range want {
		if !strings.HasPrefix(got[i], want[i]) {
			t.Fatalf("writes %q, want %q", got, want)
		}
	}
}

func TestApplyCreatesInOrder(t *testing.T) {
	f, c := newFake(t)
	f.lbStatus = "provisioning"
	m := model(t, twoPorts())
	st, err := Apply(ctx, c, testCfg, State{}, m)
	if err != nil || st.Status != "provisioning" || !st.OwnIPv4 || len(st.Groups) != 2 {
		t.Fatal(st, err)
	}
	w := f.log()
	hasPrefixes(t, w, "POST /ipv4-addresses", "POST /target-groups", "POST /target-groups", "POST /load-balancers")
	for i, key := range []string{"svc-u1-ip-", "svc-u1-http-", "svc-u1-admin-", "svc-u1-"} {
		if !strings.HasPrefix(f.keys[i], key) {
			t.Fatalf("%s key %q, want %s…", w[i], f.keys[i], key)
		}
		tags := f.bodies[i]["tags"].(map[string]any)
		if tags["ezgh:kubernetes-cluster"] != "c1d2" || tags["ezgh:kubernetes-service"] != "shop/web" {
			t.Fatal(w[i], tags)
		}
	}
	if b := f.bodies[3]; b["ipv4Address"] != st.IPv4Address || b["subnet"] != "sbn_a" {
		t.Fatal(b)
	}
	if b := f.bodies[1]; b["vpc"] != "vpc_a" || b["port"] != float64(8080) || b["protocol"] != "tcp" {
		t.Fatal(b)
	}

	// Provisioning: read again, nothing written.
	f.reset()
	if st, err = Apply(ctx, c, testCfg, st, m); err != nil || len(f.log()) != 0 || !strings.HasPrefix(st.LoadBalancer, "lbr_") {
		t.Fatal(st, err, f.log())
	}
	f.setStatus(st.LoadBalancer, "active")
	if st, err = Apply(ctx, c, testCfg, st, m); err != nil || len(st.Listeners) != 2 {
		t.Fatal(st, err)
	}
	w = f.log()
	hasPrefixes(t, w, "POST /load-balancers/"+st.LoadBalancer+"/listeners", "POST /load-balancers/"+st.LoadBalancer+"/listeners")
	if b := f.bodies[0]; b["targetGroup"] != st.Groups["u1/http"] || b["protocol"] != "tcp" || b["port"] != float64(80) {
		t.Fatal(b)
	}
}

func TestApplyIsIdempotent(t *testing.T) {
	f, c := newFake(t)
	m := model(t, twoPorts())
	st := converge(t, c, State{}, m)
	f.reset()
	again, err := Apply(ctx, c, testCfg, st, m)
	if err != nil || len(f.log()) != 0 || again.LoadBalancer != st.LoadBalancer {
		t.Fatal(err, f.log())
	}
}

func TestPortRemovedDeletesListenerThenGroup(t *testing.T) {
	f, c := newFake(t)
	st := converge(t, c, State{}, model(t, twoPorts()))
	admin := st.Groups["u1/admin"]
	f.reset()
	st, err := Apply(ctx, c, testCfg, st, model(t, service([]corev1.ServicePort{tcpPort("http", 80, intstr.FromInt32(8080))}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	hasPrefixes(t, f.log(), "DELETE /load-balancers/"+st.LoadBalancer+"/listeners/", "DELETE /target-groups/"+admin)
	if len(st.Groups) != 1 || len(st.Listeners) != 1 || st.Epoch != 1 {
		t.Fatalf("%+v", st)
	}
	// Re-adding the port makes a new group under a new key, never a replay of the deleted one.
	f.reset()
	st = converge(t, c, st, model(t, twoPorts()))
	if w := f.log(); !strings.HasPrefix(f.keys[0], "svc-u1-1-admin-") {
		t.Fatal(w, f.keys)
	}
}

func TestDeleteOrder(t *testing.T) {
	f, c := newFake(t)
	st := converge(t, c, State{}, model(t, twoPorts()))
	lb, pip := st.LoadBalancer, st.IPv4Address
	f.reset()
	st, err := Apply(ctx, c, testCfg, st, teardownModel(twoPorts()))
	if err != nil || st.Status != "deleting" || st.Empty() {
		t.Fatal(st, err)
	}
	hasPrefixes(t, f.log(), "DELETE /load-balancers/"+lb)
	// Still deleting: nothing more.
	f.reset()
	if st, err = Apply(ctx, c, testCfg, st, teardownModel(twoPorts())); err != nil || len(f.log()) != 0 {
		t.Fatal(err, f.log())
	}
	f.setStatus(lb, "deleted")
	if st, err = Apply(ctx, c, testCfg, st, teardownModel(twoPorts())); err != nil || !st.Empty() {
		t.Fatal(st, err)
	}
	hasPrefixes(t, f.log(), "DELETE /target-groups/", "DELETE /target-groups/", "DELETE /ipv4-addresses/"+pip)

	// A customer's address is never released.
	f, c = newFake(t)
	svc := twoPorts()
	svc.Annotations = map[string]string{Prefix + "ipv4-address": "pip_mine"}
	f.pips["pip_mine"] = &networkapi.IPv4Address{Slug: "pip_mine", Address: "198.51.100.7"}
	st = converge(t, c, State{}, model(t, svc))
	if st.OwnIPv4 || st.IPv4Address != "pip_mine" || st.IPv4 == nil || *st.IPv4 != "198.51.100.7" {
		t.Fatalf("%+v", st)
	}
	st, _ = Apply(ctx, c, testCfg, st, teardownModel(twoPorts()))
	f.setStatus(st.LoadBalancer, "deleted")
	f.reset()
	if st, err = Apply(ctx, c, testCfg, st, teardownModel(twoPorts())); err != nil || !st.Empty() {
		t.Fatal(st, err)
	}
	for _, w := range f.log() {
		if strings.Contains(w, "ipv4-addresses") {
			t.Fatal(f.log())
		}
	}
}

// teardownModel is the teardown model of svc, as the reconciler builds it.
func teardownModel(svc *corev1.Service) Model {
	return Model{Tags: Tags(testCfg, svc.Namespace+"/"+svc.Name), Named: ServiceLBName(svc)}
}

func TestReadRefusedDeletesNothing(t *testing.T) {
	f, c := newFake(t)
	st := converge(t, c, State{}, model(t, twoPorts()))
	f.reset()
	f.fail["GET /load-balancers/"] = [2]string{"403", "access_denied"}
	got, err := Apply(ctx, c, testCfg, st, teardownModel(twoPorts()))
	if _, ok := ezghclient.AsPermission(err); !ok || len(f.log()) != 0 || got.LoadBalancer != st.LoadBalancer || len(got.Groups) != 2 {
		t.Fatal(err, f.log(), got)
	}
}

// reconciler is a Service reconciler on a fake cluster holding objs.
func reconciler(t *testing.T, c *ezghclient.Clients, objs ...client.Object) (*ServiceReconciler, *events.FakeRecorder) {
	t.Helper()
	rec := events.NewFakeRecorder(20)
	k := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(objs...).WithStatusSubresource(&corev1.Service{}, &corev1.Pod{}).Build()
	return &ServiceReconciler{Client: k, Clients: c, Config: func() (config.Config, error) { return testCfg, nil }, Recorder: rec}, rec
}

var webKey = reconcile.Request{NamespacedName: types.NamespacedName{Namespace: "shop", Name: "web"}}

func get(t *testing.T, r *ServiceReconciler) *corev1.Service {
	t.Helper()
	var svc corev1.Service
	if err := r.Client.Get(ctx, webKey.NamespacedName, &svc); err != nil {
		t.Fatal(err)
	}
	return &svc
}

func drain(rec *events.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-rec.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func TestStatusOnlyWhenActive(t *testing.T) {
	f, c := newFake(t)
	f.lbStatus = "provisioning"
	r, _ := reconciler(t, c, twoPorts())
	res, err := r.Reconcile(ctx, webKey)
	if err != nil || res.RequeueAfter != 15*time.Second {
		t.Fatal(res, err)
	}
	svc := get(t, r)
	st, _ := ReadState(svc.Annotations)
	if len(svc.Status.LoadBalancer.Ingress) != 0 || svc.Finalizers[0] != Finalizer || st.LoadBalancer == "" {
		t.Fatalf("%+v %+v", svc.Status, svc.ObjectMeta)
	}
	f.setStatus(st.LoadBalancer, "provisioning")
	if _, err := r.Reconcile(ctx, webKey); err != nil {
		t.Fatal(err)
	}
	st, _ = ReadState(get(t, r).Annotations)
	f.mu.Lock()
	f.lbs[st.LoadBalancer].Status = "active"
	v6 := "2001:db8::9"
	f.lbs[st.LoadBalancer].IPv6Address = &v6
	f.mu.Unlock()
	if res, err = r.Reconcile(ctx, webKey); err != nil || res.RequeueAfter != 0 {
		t.Fatal(res, err)
	}
	ing := get(t, r).Status.LoadBalancer.Ingress
	if len(ing) != 2 || ing[0].IP != "203.0.113.9" || ing[1].IP != v6 || *ing[0].IPMode != corev1.LoadBalancerIPModeProxy || *ing[1].IPMode != corev1.LoadBalancerIPModeProxy {
		t.Fatalf("%+v", ing)
	}
}

func TestServiceDeniedGetsEvent(t *testing.T) {
	f, c := newFake(t)
	f.fail["POST /load-balancers"] = [2]string{"403", "access_denied"}
	r, rec := reconciler(t, c, twoPorts())
	if _, err := r.Reconcile(ctx, webKey); err == nil {
		t.Fatal("want an error, for the backoff")
	}
	ev := drain(rec)
	if len(ev) != 1 || !strings.HasPrefix(ev[0], "Warning MissingPermission ") || !strings.Contains(ev[0], "lb.loadBalancers.create") {
		t.Fatal(ev)
	}
	for _, w := range f.log() {
		if strings.HasPrefix(w, "DELETE") {
			t.Fatal("partial delete", f.log())
		}
	}
	// What was made is recorded, and the retry carries on from it.
	st, _ := ReadState(get(t, r).Annotations)
	if st.IPv4Address == "" || len(st.Groups) != 2 || st.LoadBalancer != "" {
		t.Fatalf("%+v", st)
	}
	f.reset()
	if res, err := r.Reconcile(ctx, webKey); err != nil || res.RequeueAfter != provisioningPoll {
		t.Fatal(res, err)
	}
	hasPrefixes(t, f.log(), "POST /load-balancers")
}

func TestQuotaEvent(t *testing.T) {
	for _, call := range []string{"POST /load-balancers", "POST /ipv4-addresses"} {
		for _, status := range []string{"409", "429"} {
			f, c := newFake(t)
			f.fail[call] = [2]string{status, "quota_exceeded"}
			r, rec := reconciler(t, c, twoPorts())
			if _, err := r.Reconcile(ctx, webKey); err == nil {
				t.Fatal(call, status)
			}
			if ev := drain(rec); len(ev) != 1 || !strings.HasPrefix(ev[0], "Warning QuotaExceeded This project has 2") {
				t.Fatal(call, status, ev)
			}
		}
	}
}

func TestRefusalReportedOncePerChange(t *testing.T) {
	_, c := newFake(t)
	svc := service([]corev1.ServicePort{tcpPort("http", 80, intstr.FromInt32(80)), {Name: "dns", Port: 53, Protocol: corev1.ProtocolUDP}}, nil)
	r, rec := reconciler(t, c, svc)
	for range 3 {
		if _, err := r.Reconcile(ctx, webKey); err != nil {
			t.Fatal(err)
		}
	}
	if ev := drain(rec); len(ev) != 1 || !strings.HasPrefix(ev[0], "Warning UnsupportedProtocol ") || !strings.Contains(ev[0], "53/UDP") {
		t.Fatal(ev)
	}
}

func TestTypeChangeTearsDownThenNewLifecycle(t *testing.T) {
	f, c := newFake(t)
	r, _ := reconciler(t, c, twoPorts())
	if _, err := r.Reconcile(ctx, webKey); err != nil {
		t.Fatal(err)
	}
	firstLB := f.keyOf("POST /load-balancers")
	svc := get(t, r)
	svc.Spec.Type = corev1.ServiceTypeClusterIP
	if err := r.Client.Update(ctx, svc); err != nil {
		t.Fatal(err)
	}
	st, _ := ReadState(get(t, r).Annotations)
	for range 2 {
		if _, err := r.Reconcile(ctx, webKey); err != nil {
			t.Fatal(err)
		}
		f.setStatus(st.LoadBalancer, "deleted")
	}
	svc = get(t, r)
	if len(svc.Finalizers) != 0 || len(svc.Status.LoadBalancer.Ingress) != 0 {
		t.Fatalf("%+v %+v", svc.Finalizers, svc.Status)
	}
	if st, _ = ReadState(svc.Annotations); !st.Empty() || st.Epoch == 0 {
		t.Fatalf("%+v", st)
	}
	// Back to LoadBalancer: new keys, so no create is replayed from the deleted ones.
	svc.Spec.Type = corev1.ServiceTypeLoadBalancer
	if err := r.Client.Update(ctx, svc); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, webKey); err != nil {
		t.Fatal(err)
	}
	if again := f.keyOf("POST /load-balancers"); again == firstLB || !strings.HasPrefix(again, "svc-u1-") {
		t.Fatal(firstLB, again)
	}
}

func TestDeletedServiceReleasedAfterTeardown(t *testing.T) {
	f, c := newFake(t)
	r, _ := reconciler(t, c, twoPorts())
	if _, err := r.Reconcile(ctx, webKey); err != nil {
		t.Fatal(err)
	}
	st, _ := ReadState(get(t, r).Annotations)
	if err := r.Client.Delete(ctx, get(t, r)); err != nil {
		t.Fatal(err)
	}
	if res, err := r.Reconcile(ctx, webKey); err != nil || res.RequeueAfter != provisioningPoll {
		t.Fatal(res, err)
	}
	f.setStatus(st.LoadBalancer, "deleted")
	if _, err := r.Reconcile(ctx, webKey); err != nil {
		t.Fatal(err)
	}
	var svc corev1.Service
	if err := r.Client.Get(ctx, webKey.NamespacedName, &svc); err == nil {
		t.Fatalf("still held: %+v", svc.Finalizers)
	}
	if len(f.pips) != 0 || len(f.groups) != 0 {
		t.Fatal(f.pips, f.groups)
	}
}

func TestFinalizerBeforeAnyCreate(t *testing.T) {
	f, c := newFake(t)
	f.fail["POST /ipv4-addresses"] = [2]string{"403", "access_denied"}
	r, _ := reconciler(t, c, twoPorts())
	if _, err := r.Reconcile(ctx, webKey); err == nil {
		t.Fatal("want an error")
	}
	if svc := get(t, r); len(svc.Finalizers) != 1 || svc.Finalizers[0] != Finalizer {
		t.Fatalf("%+v", svc.Finalizers)
	}
}

func TestNotFoundNeedsTheProjectToAnswer(t *testing.T) {
	f, c := newFake(t)
	st := converge(t, c, State{}, model(t, twoPorts()))
	// The group reads 404, and so does the project: a wrong project, not a deleted group.
	f.mu.Lock()
	delete(f.groups, st.Groups["u1/http"])
	f.mu.Unlock()
	f.reset()
	f.fail["GET /load-balancers"] = [2]string{"404", "not_found"}
	got, err := Apply(ctx, c, testCfg, st, model(t, twoPorts()))
	if err == nil || len(f.log()) != 0 || got.Groups["u1/http"] != st.Groups["u1/http"] {
		t.Fatal(err, f.log(), got)
	}
	// The load balancer vanished, and its replacement's create fails: the old ID stays recorded.
	f.mu.Lock()
	delete(f.lbs, st.LoadBalancer)
	f.mu.Unlock()
	f.fail["POST /load-balancers"] = [2]string{"403", "access_denied"}
	got, err = Apply(ctx, c, testCfg, st, model(t, twoPorts()))
	if err == nil || got.LoadBalancer != st.LoadBalancer || got.Epoch != st.Epoch+1 {
		t.Fatalf("%v %+v", err, got)
	}
}

func TestFailedCreateReportedNotRepeated(t *testing.T) {
	f, c := newFake(t)
	f.lbStatus = "provisioning"
	r, rec := reconciler(t, c, twoPorts())
	if _, err := r.Reconcile(ctx, webKey); err != nil {
		t.Fatal(err)
	}
	st, _ := ReadState(get(t, r).Annotations)
	f.mu.Lock()
	f.lbs[st.LoadBalancer].Status = "deleted"
	f.ops[st.Operation].Status = "failed"
	f.ops[st.Operation].Error = map[string]any{"code": "subnet_has_no_gateway_route", "message": "The subnet needs an attached Network Gateway"}
	f.mu.Unlock()
	f.reset()
	if res, err := r.Reconcile(ctx, webKey); err != nil || res.RequeueAfter != 0 {
		t.Fatal(res, err)
	}
	if ev := drain(rec); len(ev) != 1 || !strings.HasPrefix(ev[0], "Warning EZGHAPIError ") || !strings.Contains(ev[0], "subnet_has_no_gateway_route") {
		t.Fatal(ev)
	}
	if len(f.log()) != 0 {
		t.Fatal(f.log())
	}
	// Unchanged: nothing more.
	if _, err := r.Reconcile(ctx, webKey); err != nil || len(f.log()) != 0 || len(drain(rec)) != 0 {
		t.Fatal(err, f.log())
	}
	// The customer changes the Service: one new attempt.
	svc := get(t, r)
	svc.Annotations[Prefix+"subnet"] = "sbn_b"
	if err := r.Client.Update(ctx, svc); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, webKey); err != nil {
		t.Fatal(err)
	}
	hasPrefixes(t, f.log(), "POST /load-balancers")
}

func TestAdoptAfterLostState(t *testing.T) {
	f, c := newFake(t)
	m := model(t, twoPorts())
	st := converge(t, c, State{}, m)
	// Another cluster's (or Service's) group in the same project is never adopted.
	other := m
	other.Tags = map[string]string{"ezgh:kubernetes-cluster": "other", "ezgh:kubernetes-service": "shop/web"}
	stranger, _ := c.LB.CreateTargetGroup(ctx, "org_a", "prj_a", lbapi.TargetGroupCreate{Name: "k8s-shop-web-http", Tags: other.Tags, VPC: "vpc_a", Port: 80})
	f.reset()
	got, err := Apply(ctx, c, testCfg, State{}, m)
	if err != nil || len(f.log()) != 0 || got.LoadBalancer != st.LoadBalancer || got.IPv4Address != st.IPv4Address || !got.OwnIPv4 ||
		got.Groups["u1/http"] != st.Groups["u1/http"] || got.Groups["u1/admin"] != st.Groups["u1/admin"] || len(got.Listeners) != 2 {
		t.Fatalf("%v %q\n%+v\n%+v", err, f.log(), got, st)
	}
	// A lost state on a deleted Service still finds everything to delete.
	got, err = Apply(ctx, c, testCfg, State{}, Model{Tags: m.Tags, Named: m.Name})
	if err != nil || got.Status != "deleting" {
		t.Fatal(err, got)
	}
	f.setStatus(st.LoadBalancer, "deleted")
	if got, err = Apply(ctx, c, testCfg, got, Model{Tags: m.Tags, Named: m.Name}); err != nil || !got.Empty() {
		t.Fatal(err, got)
	}
	if len(f.groups) != 1 || f.groups[stranger.Slug] == nil || len(f.pips) != 0 {
		t.Fatal(f.groups, f.pips)
	}
}

func TestPortSwapDeletesListenerFirst(t *testing.T) {
	f, c := newFake(t)
	st := converge(t, c, State{}, model(t, twoPorts()))
	f.reset()
	swapped := service([]corev1.ServicePort{tcpPort("http", 80, intstr.FromInt32(8080)), tcpPort("admin", 9001, intstr.FromInt32(9000))}, nil)
	if _, err := Apply(ctx, c, testCfg, st, model(t, swapped)); err != nil {
		t.Fatal(err)
	}
	hasPrefixes(t, f.log(), "DELETE /load-balancers/"+st.LoadBalancer+"/listeners/", "POST /load-balancers/"+st.LoadBalancer+"/listeners")
}

// A v0.1.0 annotation decodes, heals, and the address is read once, not every pass.
func TestOldAnnotationHealedOnce(t *testing.T) {
	f, c := newFake(t)
	st := converge(t, c, State{}, model(t, twoPorts()))
	old, _ := ReadState(map[string]string{StateAnnotation: `{"loadBalancer":"` + st.LoadBalancer + `","ipv4Address":"` + st.IPv4Address +
		`","ownIpv4":true,"groups":{"u1/http":"` + st.Groups["u1/http"] + `","u1/admin":"` + st.Groups["u1/admin"] + `"},"status":"active","ipv4":"` + st.IPv4Address + `"}`})
	f.mu.Lock()
	f.pipGets = 0
	f.mu.Unlock()
	for range 2 {
		var err error
		if old, err = Apply(ctx, c, testCfg, old, model(t, twoPorts())); err != nil || old.IPv4 == nil || *old.IPv4 != "203.0.113.9" {
			t.Fatal(old, err)
		}
	}
	if f.pipGets != 1 {
		t.Fatal(f.pipGets)
	}
}
