package lb

import (
	"encoding/json"
	"net/netip"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/cloud/events"
)

// Prefix is every annotation's.
const Prefix = "lb.ezghcloud.com/"

type annotations struct {
	tlsPorts            map[int]bool
	certificates        []string
	proxyProtocol       string
	cidrs               []string
	subnet, ipv4Address string
	idleTimeout         *int
	deregistrationDelay *int
	healthCheck         *HealthCheck
}

// readAnnotations parses svc's lb.ezghcloud.com/ annotations; each it can't use is a refusal.
func readAnnotations(svc *corev1.Service) (annotations, []Refusal) {
	var refused []Refusal
	bad := func(name, why string) {
		refused = append(refused, Refusal{events.InvalidAnnotation, Prefix + name + ": " + why})
	}
	get := func(name string) (string, bool) {
		v, ok := svc.Annotations[Prefix+name]
		return strings.TrimSpace(v), ok
	}
	num := func(name string) *int {
		v, ok := get(name)
		if !ok {
			return nil
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			bad(name, "must be a whole number")
			return nil
		}
		return &n
	}
	list := func(v string) []string {
		var out []string
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	ids := func(name, prefix string) []string {
		v, _ := get(name)
		out := list(v)
		for _, id := range out {
			if !strings.HasPrefix(id, prefix) {
				bad(name, id+" isn't a "+prefix+" ID")
			}
		}
		return out
	}

	a := annotations{tlsPorts: map[int]bool{}, proxyProtocol: "none"}
	if v, ok := get("tls-ports"); ok {
		for _, s := range list(v) {
			n, err := strconv.Atoi(s)
			if err != nil {
				bad("tls-ports", s+" isn't a port")
				continue
			}
			a.tlsPorts[n] = true
		}
	}
	a.certificates = ids("certificates", "crt_")
	if len(a.tlsPorts) > 0 && len(a.certificates) == 0 {
		bad("tls-ports", "needs "+Prefix+"certificates")
	}
	if v, ok := get("proxy-protocol"); ok {
		if v != "v2" && v != "none" {
			bad("proxy-protocol", "must be v2")
		}
		a.proxyProtocol = v
	}
	cidrs := func(name string, vs []string) []string {
		for _, s := range vs {
			p, err := netip.ParsePrefix(s)
			if err != nil || p != p.Masked() {
				bad(name, s+" isn't a CIDR")
			}
		}
		return vs
	}
	if v, ok := get("allowed-source-cidrs"); ok {
		a.cidrs = cidrs("allowed-source-cidrs", list(v))
	} else if len(svc.Spec.LoadBalancerSourceRanges) > 0 {
		var vs []string
		for _, s := range svc.Spec.LoadBalancerSourceRanges {
			vs = append(vs, strings.TrimSpace(s))
		}
		a.cidrs = cidrs("allowed-source-cidrs (spec.loadBalancerSourceRanges)", vs)
	}
	if s := ids("subnet", "sbn_"); len(s) > 1 {
		bad("subnet", "names one subnet")
	} else if len(s) == 1 {
		a.subnet = s[0]
	}
	if s := ids("ipv4-address", "pip_"); len(s) > 1 {
		bad("ipv4-address", "names one address")
	} else if len(s) == 1 {
		a.ipv4Address = s[0]
	}
	a.idleTimeout = num("idle-timeout-seconds")
	a.deregistrationDelay = num("deregistration-delay-seconds")

	var hc HealthCheck
	set := false
	if v, ok := get("healthcheck-protocol"); ok {
		if v != "tcp" && v != "http" && v != "https" {
			bad("healthcheck-protocol", "must be tcp, http or https")
		}
		hc.Protocol, set = &v, true
	}
	if n := num("healthcheck-port"); n != nil {
		hc.Port, set = json.RawMessage(strconv.Itoa(*n)), true
	}
	if v, ok := get("healthcheck-path"); ok {
		if !strings.HasPrefix(v, "/") {
			bad("healthcheck-path", "must start with /")
		}
		hc.Path, set = &v, true
	}
	if v, ok := get("healthcheck-matcher"); ok {
		hc.Matcher, set = &v, true
	}
	for _, f := range []struct {
		name  string
		field **int
	}{
		{"healthcheck-interval-seconds", &hc.IntervalSeconds}, {"healthcheck-timeout-seconds", &hc.TimeoutSeconds},
		{"healthcheck-healthy-threshold", &hc.HealthyThreshold}, {"healthcheck-unhealthy-threshold", &hc.UnhealthyThreshold},
	} {
		if n := num(f.name); n != nil {
			*f.field, set = n, true
		}
	}
	if set {
		a.healthCheck = &hc
	}
	return a, refused
}
