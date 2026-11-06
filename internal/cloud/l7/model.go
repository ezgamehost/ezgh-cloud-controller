// Package l7 turns Ingresses (ingressClassName ezgh, grouped by lb.ezghcloud.com/group) and
// Gateway API Gateways with their HTTPRoutes (GatewayClass ezgh) into EZGH load balancers with
// http and https listeners and rules. The load balancer, pip_ and target group lifecycle is lb's
// (lb.Apply), and so are the targets: Ingress and Gateway feed lb's targets controller and
// readiness gates through lb.Feed.
package l7

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/cloud/lb"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/lbapi"
)

// Limits lb-api passes through (lb-api.json).
const (
	MaxListeners   = lb.MaxListeners // per load balancer
	MaxRules       = 100             // per listener
	MaxForward     = 5               // target groups per forward
	MaxMatch       = 5               // entries per match field
	MaxHeaderEdits = 16              // entries per header modifier list (P5)
)

// Refusal reasons beyond the shared ones (events).
const (
	RuleLimit          = "RuleLimit"
	InvalidCertificate = "InvalidCertificate"
	BackendNotFound    = "BackendNotFound"
)

// ClassName is the IngressClass's and GatewayClass's; ControllerName is both classes' controller.
const (
	ClassName      = "ezgh"
	ControllerName = "kubernetes.ezghcloud.com/gateway"
)

// Model is the L7 load balancer an Ingress group or a Gateway wants. A Model with Name "" wants
// nothing: Apply tears down what State holds (Tags find what a lost state left behind).
type Model struct {
	// Key starts every idempotency key of the owner's creates.
	Key  string
	Name string
	// Named is the load balancer's name when Name is "" (tearing down), for checking what state names.
	Named     string
	Tags      map[string]string
	Listeners []Listener
	Backends  []Backend
	// Secrets are the valid kubernetes.io/tls Secrets listeners import, by "<ns>/<name>".
	Secrets map[string]*corev1.Secret
}

// Listener is one http or https listener: its rules in precedence order, then Default.
type Listener struct {
	Port     int
	Protocol string // http | https
	// Certificates are crt_ IDs named outright; Secrets ("<ns>/<name>") are imported, and follow
	// them. The first certificate is the default, the rest are picked by SNI.
	Certificates []string
	Secrets      []string
	Default      Action
	Rules        []Rule
}

// Rule is one lb rule: every field ANDs, entries within Hosts, Paths and Methods OR, and every
// header and query entry must match (lb-api renders them as Gateway API's HTTPRoute does).
type Rule struct {
	// Source names the object it came from ("Ingress shop/web"), for refusals.
	Source  string
	Hosts   []string
	Paths   []PathMatch
	Methods []string
	Headers []NameValue
	Query   []NameValue
	Action  Action
}

// PathMatch, NameValue and HeaderModifier are lb-api's.
type (
	PathMatch      = lbapi.PathMatch
	NameValue      = lbapi.NameValue
	HeaderModifier = lbapi.HeaderModifier
	Redirect       = lbapi.L7Redirect
	Fixed          = lbapi.L7FixedResponse
)

// Action is a forward to backends by weight, a redirect or a fixed response. Header modifiers
// go with a forward only (P5).
type Action struct {
	Type            string // forward | redirect | fixedResponse
	Forward         []Weighted
	Redirect        *Redirect
	Fixed           *Fixed
	RequestHeaders  *HeaderModifier
	ResponseHeaders *HeaderModifier
}

// Weighted is a forward's backend (a Backend.Key) and its weight.
type Weighted struct {
	Backend string
	Weight  int
}

// Backend is an http target group fed from one Service port.
type Backend struct {
	Key         string // BackendKey
	Service     string // "<ns>/<name>"
	Port        int    // the Service port
	HealthCheck *lb.HealthCheck
}

// BackendKey names the group fed from a Service port. Namespace and Service names hold no dots,
// so the key is unique within a load balancer and can be read back (ParseBackendKey).
func BackendKey(ns, svc string, port int) string { return fmt.Sprintf("%s.%s.%d", ns, svc, port) }

// ParseBackendKey reads a BackendKey.
func ParseBackendKey(key string) (ns, svc string, port int, ok bool) {
	parts := strings.Split(key, ".")
	if len(parts) != 3 {
		return "", "", 0, false
	}
	if _, err := fmt.Sscan(parts[2], &port); err != nil {
		return "", "", 0, false
	}
	return parts[0], parts[1], port, true
}

func fixed(status int) Action {
	return Action{Type: "fixedResponse", Fixed: &Fixed{StatusCode: status, ContentType: "text/plain"}}
}

func forward(to ...Weighted) Action { return Action{Type: "forward", Forward: to} }

// addBackend records b once.
func (m *Model) addBackend(b Backend) {
	if !slices.ContainsFunc(m.Backends, func(x Backend) bool { return x.Key == b.Key }) {
		m.Backends = append(m.Backends, b)
	}
}

// limit sorts every listener's rules by precedence and keeps what lb-api takes: MaxListeners
// listeners (by port) and MaxRules rules each. What's cut is refused by name; the rest applies.
func (m *Model) limit() []lb.Refusal {
	var refused []lb.Refusal
	slices.SortFunc(m.Listeners, func(a, b Listener) int { return a.Port - b.Port })
	if len(m.Listeners) > MaxListeners {
		var ports []string
		for _, l := range m.Listeners[MaxListeners:] {
			ports = append(ports, fmt.Sprint(l.Port))
		}
		refused = append(refused, lb.Refusal{Reason: "TooManyListeners", Message: fmt.Sprintf("An EZGH load balancer has at most %d listeners; ports %s aren't served", MaxListeners, strings.Join(ports, ", "))})
		m.Listeners = m.Listeners[:MaxListeners]
	}
	for i := range m.Listeners {
		l := &m.Listeners[i]
		Sort(l.Rules)
		if len(l.Rules) <= MaxRules {
			continue
		}
		var from []string
		for _, r := range l.Rules[MaxRules:] {
			if !slices.Contains(from, r.Source) {
				from = append(from, r.Source)
			}
		}
		refused = append(refused, lb.Refusal{Reason: RuleLimit, Message: fmt.Sprintf("Port %d has %d rules; an EZGH listener takes %d, so the lowest-precedence %d aren't served (from %s)",
			l.Port, len(l.Rules), MaxRules, len(l.Rules)-MaxRules, strings.Join(from, ", "))})
		l.Rules = l.Rules[:MaxRules]
	}
	// Backends only the cut rules used aren't made.
	used := map[string]bool{}
	for _, l := range m.Listeners {
		for _, a := range append([]Action{l.Default}, actions(l.Rules)...) {
			for _, w := range a.Forward {
				used[w.Backend] = true
			}
		}
	}
	m.Backends = slices.DeleteFunc(m.Backends, func(b Backend) bool { return !used[b.Key] })
	return refused
}

func actions(rules []Rule) []Action {
	out := make([]Action, len(rules))
	for i, r := range rules {
		out[i] = r.Action
	}
	return out
}

// checkMatch refuses a match lb-api wouldn't take: more than MaxMatch entries in a field.
func checkMatch(r Rule) string {
	for _, f := range []struct {
		name string
		n    int
	}{{"paths", len(r.Paths)}, {"methods", len(r.Methods)}, {"headers", len(r.Headers)}, {"query parameters", len(r.Query)}} {
		if f.n > MaxMatch {
			return fmt.Sprintf("a match has %d %s; an EZGH rule takes %d", f.n, f.name, MaxMatch)
		}
	}
	if len(r.Action.Forward) > MaxForward {
		return fmt.Sprintf("a rule forwards to %d backends; an EZGH rule takes %d", len(r.Action.Forward), MaxForward)
	}
	return ""
}

// splitHosts makes one rule per host specificity (exact names, then each wildcard length) and at
// most MaxMatch hosts each, so precedence orders every host correctly. No hosts stays one rule.
func splitHosts(r Rule) []Rule {
	if len(r.Hosts) == 0 {
		return []Rule{r}
	}
	byRank := map[int][]string{}
	for _, h := range r.Hosts {
		byRank[hostRank([]string{h})] = append(byRank[hostRank([]string{h})], h)
	}
	var out []Rule
	for _, rank := range slices.Sorted(maps.Keys(byRank)) {
		hosts := byRank[rank]
		slices.Sort(hosts)
		for c := range slices.Chunk(slices.Compact(hosts), MaxMatch) {
			x := r
			x.Hosts = c
			out = append(out, x)
		}
	}
	return out
}
