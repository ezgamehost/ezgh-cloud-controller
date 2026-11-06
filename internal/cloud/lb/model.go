// Package lb turns LoadBalancer Services into EZGH load balancers (L4), keeps their target groups'
// targets in step with EndpointSlices, and holds pods NotReady behind readiness gates until the
// load balancer's health checks pass. The load balancer, pip_ and target group lifecycle (Apply)
// and the targets (Targets, SyncTargets, Backends) are shared with the L7 controllers.
package lb

import (
	"cmp"
	"context"
	"fmt"
	"path"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/cloud/config"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/cloud/events"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/lbapi"
)

// Limits lb-api passes through (spec "Load balancers").
const (
	MaxListeners = 10
	MaxTargets   = 500
)

// Model is the load balancer a Service (or, in L7, an Ingress group or Gateway) wants. A Model with
// Name "" wants nothing: Apply tears down what State holds, and its Tags (when set) find what a
// lost state annotation left behind.
type Model struct {
	// Key starts every idempotency key of this owner's creates: svc-<uid>.
	Key  string
	Name string
	// Named is the load balancer's name when Name is "" (tearing down): what adoption and every
	// ID read from state are checked against, beside Tags.
	Named       string
	Subnet      string
	IPv4Address string // the customer's pip_, or "" for one the controller allocates
	Tags        map[string]string
	// Groups are every target group the owner wants; Apply deletes the rest once no listener
	// uses them.
	Groups    []GroupModel
	Listeners []Listener
	// SyncListeners, when set, makes the listeners instead of Listeners (L7's http and https
	// listeners). Apply calls it once the load balancer is active and every group exists, and
	// deletes the groups no longer wanted after it.
	SyncListeners func(ctx context.Context, st *State) error
}

// LBName is the load balancer's name, also when tearing down.
func (m Model) LBName() string { return cmp.Or(m.Name, m.Named) }

// Owns reports whether a resource with tags and name is m's: both of m's tags (the cluster's and
// the owner's) and its name, the load balancer's exactly (a load balancer, pip_ or certificate) or,
// for a group, starting with it and "-". Every ID read from a state annotation is checked so before
// anything changes it: whoever can edit the object can write its annotation, naming anything.
func (m Model) Owns(tags map[string]string, name string, group bool) bool {
	n := m.LBName()
	if n == "" || len(m.Tags) == 0 {
		return false
	}
	for k, v := range m.Tags {
		if tags[k] != v {
			return false
		}
	}
	if group {
		return strings.HasPrefix(name, cut(n+"-")) && name != n || len(n) >= 100 && name == n
	}
	return name == n
}

// Listener is one listener and the target group it forwards to.
type Listener struct {
	Port               int
	Protocol           string // tcp | tls
	Certificates       []string
	AllowedSourceCidrs []string
	IdleTimeoutSeconds *int
	Group              string // the GroupModel.Key it forwards to
}

// GroupModel is a target group. Key is "<owner uid>/<Service port name, or number when unnamed>".
type GroupModel struct {
	Key                        string
	Protocol                   string // tcp | http
	Port                       int
	ProxyProtocol              string // none | v2
	HealthCheck                *HealthCheck
	DeregistrationDelaySeconds *int
}

// HealthCheck is lb-api's own input: only what's set is sent and compared.
type HealthCheck = lbapi.HealthCheckInput

// Refusal is something the controller can't serve, for an event on the object.
type Refusal struct{ Reason, Message string }

// Handled reports whether svc is ours: type LoadBalancer with no loadBalancerClass (another class
// belongs to someone else's controller).
func Handled(svc *corev1.Service) bool {
	return svc.Spec.Type == corev1.ServiceTypeLoadBalancer && svc.Spec.LoadBalancerClass == nil
}

// Desired is the load balancer svc asks for. TCP ports are served; others are refused by name. An
// annotation it can't use, or more than MaxListeners TCP ports, refuses the whole Service (no
// model), so a typo never changes what's serving.
func Desired(svc *corev1.Service, cfg config.Config) (Model, []Refusal) {
	if !Handled(svc) {
		return Model{}, nil
	}
	a, refusals := readAnnotations(svc)
	var tcp []corev1.ServicePort
	var other []string
	for _, p := range svc.Spec.Ports {
		if p.Protocol == "" || p.Protocol == corev1.ProtocolTCP {
			tcp = append(tcp, p)
		} else {
			other = append(other, fmt.Sprintf("%d/%s", p.Port, p.Protocol))
		}
	}
	if len(other) > 0 {
		refusals = append(refusals, Refusal{events.UnsupportedProtocol, "EZGH load balancers serve TCP only; not served: " + strings.Join(other, ", ")})
	}
	for port := range a.tlsPorts {
		if !hasPort(tcp, port) {
			refusals = append(refusals, Refusal{events.InvalidAnnotation, fmt.Sprintf("%stls-ports: %d isn't one of the Service's TCP ports", Prefix, port)})
		}
	}
	if len(tcp) > MaxListeners {
		refusals = append(refusals, Refusal{events.TooManyListeners, fmt.Sprintf("The Service has %d TCP ports; an EZGH load balancer has at most %d listeners", len(tcp), MaxListeners)})
	}
	for _, r := range refusals {
		if r.Reason != events.UnsupportedProtocol {
			return Model{}, refusals
		}
	}
	if len(tcp) == 0 {
		return Model{}, refusals
	}
	m := Model{
		Key:         "svc-" + string(svc.UID),
		Name:        ServiceLBName(svc),
		Subnet:      a.subnet,
		IPv4Address: a.ipv4Address,
		Tags:        Tags(cfg, svc.Namespace+"/"+svc.Name),
	}
	if m.Subnet == "" {
		m.Subnet = cfg.Subnet
	}
	for _, p := range tcp {
		g := GroupModel{Key: GroupKey(svc, p), Protocol: "tcp", Port: int(p.Port), ProxyProtocol: a.proxyProtocol, HealthCheck: a.healthCheck, DeregistrationDelaySeconds: a.deregistrationDelay}
		if p.TargetPort.IntVal > 0 {
			g.Port = int(p.TargetPort.IntVal)
		}
		l := Listener{Port: int(p.Port), Protocol: "tcp", AllowedSourceCidrs: a.cidrs, IdleTimeoutSeconds: a.idleTimeout, Group: g.Key}
		if a.tlsPorts[int(p.Port)] {
			l.Protocol, l.Certificates = "tls", a.certificates
		}
		m.Groups = append(m.Groups, g)
		m.Listeners = append(m.Listeners, l)
	}
	return m, refusals
}

// ServiceLBName is a Service's load balancer's name.
func ServiceLBName(svc *corev1.Service) string { return cut("k8s-" + svc.Namespace + "-" + svc.Name) }

// Tags are the two ezgh: tags the cluster's Bot may write (D2), on the load balancer, its groups
// and an owned pip_.
func Tags(cfg config.Config, owner string) map[string]string {
	t := cfg.ClusterTags()
	t["ezgh:kubernetes-service"] = owner
	return t
}

// GroupKey is the State.Groups key of the group fed from svc's port p.
func GroupKey(svc *corev1.Service, p corev1.ServicePort) string {
	name := p.Name
	if name == "" {
		name = fmt.Sprint(p.Port)
	}
	return string(svc.UID) + "/" + name
}

// groupSuffix is a group key's last part, for its idempotency key (svc-<uid>-<port>).
func groupSuffix(key string) string { return path.Base(key) }

func hasPort(ports []corev1.ServicePort, port int) bool {
	for _, p := range ports {
		if int(p.Port) == port {
			return true
		}
	}
	return false
}

// cut keeps a name within lb-api's 100 characters.
func cut(s string) string {
	if r := []rune(s); len(r) > 100 {
		return string(r[:100])
	}
	return s
}
