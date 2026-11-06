package lb

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/cloud/config"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/lbapi"
)

type ep struct {
	ip, pod            string
	ready, terminating bool
}

func slice(name string, family discoveryv1.AddressType, portName string, port int32, eps ...ep) discoveryv1.EndpointSlice {
	s := discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Namespace: "shop", Name: name, Labels: map[string]string{discoveryv1.LabelServiceName: "web"}},
		AddressType: family,
		Ports:       []discoveryv1.EndpointPort{{Name: &portName, Port: &port}},
	}
	for _, e := range eps {
		s.Endpoints = append(s.Endpoints, discoveryv1.Endpoint{
			Addresses:  []string{e.ip},
			Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(e.ready), Terminating: ptr.To(e.terminating)},
			TargetRef:  &corev1.ObjectReference{Kind: "Pod", Namespace: "shop", Name: e.pod},
		})
	}
	return s
}

// pod is a web pod at ip with conditions set to status (ContainersReady first, then each gate).
func pod(name, ip string, containersReady bool, gates map[string]corev1.ConditionStatus) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: name, Labels: map[string]string{"app": "web"}},
		Status:     corev1.PodStatus{PodIP: ip, PodIPs: []corev1.PodIP{{IP: ip}}},
	}
	cr := corev1.ConditionFalse
	if containersReady {
		cr = corev1.ConditionTrue
	}
	p.Status.Conditions = append(p.Status.Conditions, corev1.PodCondition{Type: corev1.ContainersReady, Status: cr}, corev1.PodCondition{Type: corev1.PodReady, Status: corev1.ConditionFalse})
	for g, st := range gates {
		p.Spec.ReadinessGates = append(p.Spec.ReadinessGates, corev1.PodReadinessGate{ConditionType: corev1.PodConditionType(g)})
		if st != "" {
			p.Status.Conditions = append(p.Status.Conditions, corev1.PodCondition{Type: corev1.PodConditionType(g), Status: st})
		}
	}
	return p
}

func TestTargetsReadyOnly(t *testing.T) {
	got, over := Targets([]discoveryv1.EndpointSlice{slice("a", discoveryv1.AddressTypeIPv4, "http", 8080,
		ep{ip: "10.0.0.3", pod: "c", ready: true}, ep{ip: "10.0.0.1", pod: "a", ready: false}, ep{ip: "10.0.0.2", pod: "b", ready: true, terminating: true},
		ep{ip: "10.0.0.10", pod: "d", ready: true})}, "http", nil)
	if over != 0 || fmt.Sprint(got) != "[{10.0.0.3 8080} {10.0.0.10 8080}]" {
		t.Fatal(got, over)
	}
}

func TestRegistersPodsHeldOnlyByOwnGate(t *testing.T) {
	ours := GateType("web")
	pods := map[string]*corev1.Pod{
		"held":        pod("held", "10.0.0.1", true, map[string]corev1.ConditionStatus{ours: ""}),
		"also-ours":   pod("also-ours", "10.0.0.2", true, map[string]corev1.ConditionStatus{ours: corev1.ConditionFalse, GateType("other"): ""}),
		"others-gate": pod("others-gate", "10.0.0.3", true, map[string]corev1.ConditionStatus{ours: "", "example.com/ready": corev1.ConditionFalse}),
		"not-ready":   pod("not-ready", "10.0.0.4", false, map[string]corev1.ConditionStatus{ours: ""}),
	}
	var eps []ep
	for name, p := range pods {
		eps = append(eps, ep{ip: p.Status.PodIP, pod: name})
	}
	got, _ := Targets([]discoveryv1.EndpointSlice{slice("a", discoveryv1.AddressTypeIPv4, "http", 80, eps...)}, "http", pods)
	if fmt.Sprint(got) != "[{10.0.0.1 80} {10.0.0.2 80}]" {
		t.Fatal(got)
	}
}

func TestPerTargetPort(t *testing.T) {
	got, _ := Targets([]discoveryv1.EndpointSlice{
		slice("a", discoveryv1.AddressTypeIPv4, "http", 8080, ep{ip: "10.0.0.1", pod: "a", ready: true}),
		slice("b", discoveryv1.AddressTypeIPv4, "http", 9090, ep{ip: "10.0.0.2", pod: "b", ready: true}),
		slice("c", discoveryv1.AddressTypeIPv4, "admin", 7000, ep{ip: "10.0.0.3", pod: "c", ready: true}),
	}, "http", nil)
	if fmt.Sprint(got) != "[{10.0.0.1 8080} {10.0.0.2 9090}]" {
		t.Fatal(got)
	}
}

func TestSyncTargetsDiff(t *testing.T) {
	f, c := newFake(t)
	g, _ := c.LB.CreateTargetGroup(ctx, "org_a", "prj_a", lbapi.TargetGroupCreate{Name: "g", VPC: "vpc_a", Port: 80})
	if _, err := SyncTargets(ctx, c, testCfg, g.Slug, []Target{{"10.0.0.1", 80}, {"10.0.0.2", 80}}); err != nil {
		t.Fatal(err)
	}
	f.reset()
	f.fail["POST /target-groups/"+g.Slug+"/targets"] = [2]string{"409", "target_already_registered"}
	got, err := SyncTargets(ctx, c, testCfg, g.Slug, []Target{{"10.0.0.2", 80}, {"10.0.0.3", 80}, {"10.0.0.4", 80}})
	if err != nil {
		t.Fatal(err)
	}
	// 10.0.0.1 goes; 10.0.0.3 answers already registered (done); 10.0.0.4 is registered.
	hasPrefixes(t, f.log(), "DELETE /target-groups/"+g.Slug+"/targets/", "POST /target-groups/"+g.Slug+"/targets")
	if len(got) != 2 {
		t.Fatal(got)
	}
	f.reset()
	if _, err := SyncTargets(ctx, c, testCfg, g.Slug, []Target{{"10.0.0.2", 80}, {"10.0.0.4", 80}}); err != nil || len(f.log()) != 0 {
		t.Fatal(err, f.log())
	}
}

// targetsSetup is an active load balancer for svc, and a targets reconciler on a cluster holding
// svc (with its state) and objs.
func targetsSetup(t *testing.T, svc *corev1.Service, objs ...client.Object) (*fakeEZGH, *TargetsReconciler, *events.FakeRecorder, State) {
	t.Helper()
	f, c := newFake(t)
	st := converge(t, c, State{}, model(t, svc))
	data, _ := json.Marshal(st)
	svc.Annotations = map[string]string{StateAnnotation: string(data)}
	r, rec := reconciler(t, c, append(objs, svc)...)
	return f, &TargetsReconciler{Client: r.Client, Clients: c, Config: func() (config.Config, error) { return testCfg, nil }, Recorder: rec, Backends: Backends{ServiceBackends}}, rec, st
}

func TestTargetsCapAt500(t *testing.T) {
	var eps []ep
	for i := range 600 {
		eps = append(eps, ep{ip: fmt.Sprintf("10.0.%d.%d", i/250, i%250+1), pod: fmt.Sprint("p", i), ready: true})
	}
	s := slice("a", discoveryv1.AddressTypeIPv4, "http", 8080, eps...)
	got, over := Targets([]discoveryv1.EndpointSlice{s}, "http", nil)
	if len(got) != 500 || over != 100 || got[0].Address != "10.0.0.1" {
		t.Fatal(len(got), over)
	}
	f, r, rec, st := targetsSetup(t, service([]corev1.ServicePort{tcpPort("http", 80, intstr.FromInt32(8080))}, nil), &s)
	for range 2 {
		if _, err := r.Reconcile(ctx, webKey); err != nil {
			t.Fatal(err)
		}
	}
	if ev := drain(rec); len(ev) != 1 || !strings.HasPrefix(ev[0], "Warning TargetLimit 100 endpoints") {
		t.Fatal(ev)
	}
	n := 0
	for _, tg := range f.targets {
		if tg.TargetGroup == st.Groups["u1/http"] {
			n++
		}
	}
	if n != 500 {
		t.Fatal(n)
	}
}

func TestIPv6SliceIgnored(t *testing.T) {
	s := slice("a", discoveryv1.AddressTypeIPv6, "http", 8080, ep{ip: "fd00::1", pod: "a", ready: true})
	if got, _ := Targets([]discoveryv1.EndpointSlice{s}, "http", nil); len(got) != 0 {
		t.Fatal(got)
	}
	svc := service([]corev1.ServicePort{tcpPort("http", 80, intstr.FromInt32(8080))}, nil)
	svc.Spec.IPFamilies = []corev1.IPFamily{corev1.IPv6Protocol}
	f, r, rec, _ := targetsSetup(t, svc, &s)
	f.reset()
	if _, err := r.Reconcile(ctx, webKey); err != nil {
		t.Fatal(err)
	}
	if ev := drain(rec); len(ev) != 1 || !strings.HasPrefix(ev[0], "Warning UnsupportedProtocol ") || !strings.Contains(ev[0], "IPv6 targets") || len(f.log()) != 0 {
		t.Fatal(ev, f.log())
	}
}
