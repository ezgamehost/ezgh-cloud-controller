package lb

import (
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/cloud/config"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/cloud/events"
)

var testCfg = config.Config{Cluster: "kcl_a", ClusterUUID: "c1d2", Org: "org_a", Project: "prj_a", VPC: "vpc_a", Subnet: "sbn_a"}

func service(ports []corev1.ServicePort, ann map[string]string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web", UID: "u1", Annotations: ann},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer, Ports: ports, Selector: map[string]string{"app": "web"}, IPFamilies: []corev1.IPFamily{corev1.IPv4Protocol}},
	}
}

func tcpPort(name string, port int32, target intstr.IntOrString) corev1.ServicePort {
	return corev1.ServicePort{Name: name, Port: port, Protocol: corev1.ProtocolTCP, TargetPort: target}
}

func TestDesiredTCP(t *testing.T) {
	m, refused := Desired(service([]corev1.ServicePort{tcpPort("http", 80, intstr.FromInt32(8080)), tcpPort("admin", 9000, intstr.FromString("admin"))}, nil), testCfg)
	if len(refused) != 0 || len(m.Listeners) != 2 {
		t.Fatal(m, refused)
	}
	if m.Name != "k8s-shop-web" || m.Key != "svc-u1" || m.Subnet != "sbn_a" ||
		m.Tags["ezgh:kubernetes-cluster"] != "c1d2" || m.Tags["ezgh:kubernetes-service"] != "shop/web" || len(m.Tags) != 2 {
		t.Fatalf("%+v", m)
	}
	l0, g0, g1 := m.Listeners[0], m.Groups[0], m.Groups[1]
	if l0.Port != 80 || l0.Protocol != "tcp" || l0.Group != "u1/http" || g0.Key != "u1/http" || g0.Protocol != "tcp" || g0.Port != 8080 || g0.ProxyProtocol != "none" {
		t.Fatalf("%+v %+v", l0, g0)
	}
	if len(m.Groups) != 2 || m.Listeners[1].Group != "u1/admin" || g1.Key != "u1/admin" || g1.Port != 9000 {
		t.Fatalf("%+v", m)
	}
}

func TestDesiredTLSPorts(t *testing.T) {
	m, refused := Desired(service([]corev1.ServicePort{tcpPort("https", 443, intstr.FromInt32(8443))},
		map[string]string{Prefix + "tls-ports": "443", Prefix + "certificates": "crt_a, crt_b"}), testCfg)
	if len(refused) != 0 || m.Listeners[0].Protocol != "tls" || strings.Join(m.Listeners[0].Certificates, ",") != "crt_a,crt_b" {
		t.Fatal(m, refused)
	}
	m, refused = Desired(service([]corev1.ServicePort{tcpPort("https", 443, intstr.FromInt32(8443))}, map[string]string{Prefix + "tls-ports": "443"}), testCfg)
	if m.Name != "" || len(refused) != 1 || refused[0].Reason != events.InvalidAnnotation {
		t.Fatal(m, refused)
	}
}

func TestUDPPortRefusedTCPServed(t *testing.T) {
	dns := corev1.ServicePort{Name: "dns", Port: 53, Protocol: corev1.ProtocolUDP}
	m, refused := Desired(service([]corev1.ServicePort{tcpPort("http", 80, intstr.FromInt32(80)), dns}, nil), testCfg)
	if len(m.Listeners) != 1 || m.Listeners[0].Port != 80 || len(refused) != 1 || refused[0].Reason != events.UnsupportedProtocol || !strings.Contains(refused[0].Message, "53/UDP") {
		t.Fatal(m, refused)
	}
	m, refused = Desired(service([]corev1.ServicePort{dns}, nil), testCfg)
	if m.Name != "" || len(refused) != 1 || refused[0].Reason != events.UnsupportedProtocol || !strings.Contains(refused[0].Message, "53/UDP") {
		t.Fatal(m, refused)
	}
}

func TestElevenPortsRefused(t *testing.T) {
	var ports []corev1.ServicePort
	for i := range 11 {
		ports = append(ports, tcpPort(fmt.Sprint("p", i), int32(8000+i), intstr.FromInt32(80)))
	}
	m, refused := Desired(service(ports, nil), testCfg)
	if m.Name != "" || len(refused) != 1 || refused[0].Reason != events.TooManyListeners {
		t.Fatal(m, refused)
	}
}

func TestOtherClassIgnored(t *testing.T) {
	svc := service([]corev1.ServicePort{tcpPort("http", 80, intstr.FromInt32(80))}, nil)
	svc.Spec.LoadBalancerClass = ptr.To("example.com/other")
	if m, refused := Desired(svc, testCfg); m.Name != "" || refused != nil || Handled(svc) {
		t.Fatal(m, refused)
	}
	svc = service([]corev1.ServicePort{tcpPort("http", 80, intstr.FromInt32(80))}, nil)
	svc.Spec.Type = corev1.ServiceTypeClusterIP
	if m, _ := Desired(svc, testCfg); m.Name != "" {
		t.Fatal(m)
	}
}

func TestAnnotationValidation(t *testing.T) {
	for _, tc := range []struct{ name, key, value, want string }{
		{"bad CIDR", "allowed-source-cidrs", "10.0.0.0/8, 10.1.2.3/8", "10.1.2.3/8"},
		{"proxy v1", "proxy-protocol", "v1", "proxy-protocol"},
		{"not the Service's port", "tls-ports", "8443", "8443"},
		{"subnet prefix", "subnet", "sub_x", "sub_x"},
		{"address prefix", "ipv4-address", "1.2.3.4", "1.2.3.4"},
		{"number", "idle-timeout-seconds", "soon", "idle-timeout-seconds"},
		{"health protocol", "healthcheck-protocol", "udp", "healthcheck-protocol"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ann := map[string]string{Prefix + tc.key: tc.value, Prefix + "certificates": "crt_a"}
			m, refused := Desired(service([]corev1.ServicePort{tcpPort("http", 80, intstr.FromInt32(80))}, ann), testCfg)
			if m.Name != "" || len(refused) != 1 || refused[0].Reason != events.InvalidAnnotation || !strings.Contains(refused[0].Message, tc.want) {
				t.Fatal(m, refused)
			}
		})
	}
	// All of them, valid.
	ann := map[string]string{
		Prefix + "proxy-protocol": "v2", Prefix + "allowed-source-cidrs": "10.0.0.0/8,2001:db8::/32", Prefix + "subnet": "sbn_b",
		Prefix + "ipv4-address": "pip_a", Prefix + "idle-timeout-seconds": "600", Prefix + "deregistration-delay-seconds": "30",
		Prefix + "healthcheck-protocol": "http", Prefix + "healthcheck-path": "/healthz", Prefix + "healthcheck-port": "9000",
		Prefix + "healthcheck-interval-seconds": "10", Prefix + "healthcheck-timeout-seconds": "5",
		Prefix + "healthcheck-healthy-threshold": "2", Prefix + "healthcheck-unhealthy-threshold": "3", Prefix + "healthcheck-matcher": "200",
	}
	m, refused := Desired(service([]corev1.ServicePort{tcpPort("http", 80, intstr.FromInt32(80))}, ann), testCfg)
	l, g := m.Listeners[0], m.Groups[0]
	hc := g.HealthCheck
	if len(refused) != 0 || m.Subnet != "sbn_b" || m.IPv4Address != "pip_a" || g.ProxyProtocol != "v2" || *l.IdleTimeoutSeconds != 600 ||
		*g.DeregistrationDelaySeconds != 30 || len(l.AllowedSourceCidrs) != 2 || *hc.Protocol != "http" || *hc.Path != "/healthz" ||
		string(hc.Port) != "9000" || *hc.IntervalSeconds != 10 || *hc.TimeoutSeconds != 5 || *hc.HealthyThreshold != 2 || *hc.UnhealthyThreshold != 3 || *hc.Matcher != "200" {
		t.Fatal(m, refused)
	}
	// spec.loadBalancerSourceRanges when the annotation is absent.
	svc := service([]corev1.ServicePort{tcpPort("http", 80, intstr.FromInt32(80))}, nil)
	svc.Spec.LoadBalancerSourceRanges = []string{"192.0.2.0/24"}
	if m, _ := Desired(svc, testCfg); strings.Join(m.Listeners[0].AllowedSourceCidrs, ",") != "192.0.2.0/24" {
		t.Fatal(m)
	}
}
