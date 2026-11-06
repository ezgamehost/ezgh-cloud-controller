package lb

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/lbapi"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/networkapi"
)

type apiKey string

func (k apiKey) APIKey(context.Context) (string, error) { return string(k), nil }

// fakeEZGH is lb-api and network-api in memory. Writes are logged as "METHOD /path…" below the
// project, with the idempotency key and body kept per write.
type fakeEZGH struct {
	mu        sync.Mutex
	n         int
	lbs       map[string]*lbapi.LoadBalancer
	listeners map[string]*lbapi.Listener
	groups    map[string]*lbapi.TargetGroup
	targets   map[string]*lbapi.Target
	pips      map[string]*networkapi.IPv4Address
	ops       map[string]*lbapi.Operation
	writes    []string
	keys      []string         // each write's Idempotency-Key
	bodies    []map[string]any // each write's body
	// fail answers the first request whose "METHOD /path" starts with a key with that status and code.
	fail map[string][2]string
	// lbStatus is what a new load balancer's status is.
	lbStatus string
}

func newFake(t *testing.T) (*fakeEZGH, *ezghclient.Clients) {
	f := &fakeEZGH{lbs: map[string]*lbapi.LoadBalancer{}, listeners: map[string]*lbapi.Listener{}, groups: map[string]*lbapi.TargetGroup{},
		targets: map[string]*lbapi.Target{}, pips: map[string]*networkapi.IPv4Address{}, ops: map[string]*lbapi.Operation{}, fail: map[string][2]string{}, lbStatus: "active"}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, ezghclient.NewAt(apiKey("ezgh_test"), "test", func(string) string { return srv.URL })
}

func (f *fakeEZGH) id(prefix string) string { f.n++; return fmt.Sprintf("%s_%d", prefix, f.n) }

func (f *fakeEZGH) reset() { f.mu.Lock(); f.writes, f.keys, f.bodies = nil, nil, nil; f.mu.Unlock() }

func (f *fakeEZGH) setStatus(lb, status string) {
	f.mu.Lock()
	f.lbs[lb].Status = status
	f.mu.Unlock()
}

func (f *fakeEZGH) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.writes...)
}

func (f *fakeEZGH) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/v1/organizations/org_a/projects/prj_a")
	call := r.Method + " " + path
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	for prefix, v := range f.fail {
		if strings.HasPrefix(call, prefix) {
			delete(f.fail, prefix)
			status := map[string]int{"403": 403, "409": 409, "429": 429, "404": 404}[v[0]]
			msg := "refused"
			if v[1] == "access_denied" {
				msg = "This API key isn't allowed to lb.loadBalancers.create"
			}
			if v[1] == "quota_exceeded" {
				msg = "This project has 2 load balancers, its limit (lb.loadBalancersPerProject)"
			}
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": v[1], "message": msg}})
			return
		}
	}
	if r.Method != http.MethodGet {
		f.writes = append(f.writes, call)
		f.keys = append(f.keys, r.Header.Get("Idempotency-Key"))
		f.bodies = append(f.bodies, body)
	}
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); r.URL.Query().Has("limit") && (err != nil || n < 1 || n > 100) {
		w.WriteHeader(400) // lb-api and network-api take 1-100
		_, _ = w.Write([]byte(`{"error":{"code":"invalid_request","message":"limit must be 1-100"}}`))
		return
	}
	seg := strings.Split(strings.Trim(path, "/"), "/")
	str := func(k string) string { s, _ := body[k].(string); return s }
	tags := func() map[string]string {
		out := map[string]string{}
		m, _ := body["tags"].(map[string]any)
		for k, v := range m {
			out[k] = v.(string)
		}
		return out
	}
	num := func(k string) int { n, _ := body[k].(float64); return int(n) }
	notFound := func() {
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"not found"}}`))
	}
	out := func(status int, v any) { w.WriteHeader(status); _ = json.NewEncoder(w).Encode(v) }
	switch {
	case strings.HasPrefix(call, "GET /v1/organizations/org_a/operations/"):
		op := f.ops[seg[len(seg)-1]]
		if op == nil {
			notFound()
			return
		}
		out(200, op)
	case call == "GET /load-balancers":
		var lbs []lbapi.LoadBalancer
		for k, lb := range f.lbs {
			if k == lb.Slug {
				lbs = append(lbs, *lb)
			}
		}
		out(200, map[string]any{"loadBalancers": lbs, "nextCursor": nil})
	case call == "GET /target-groups":
		var gs []lbapi.TargetGroup
		for _, g := range f.groups {
			gs = append(gs, *g)
		}
		out(200, map[string]any{"targetGroups": gs, "nextCursor": nil})
	case call == "GET /ipv4-addresses":
		var ps []networkapi.IPv4Address
		for _, p := range f.pips {
			ps = append(ps, *p)
		}
		out(200, map[string]any{"ipv4Addresses": ps, "nextCursor": nil})
	case call == "POST /ipv4-addresses":
		p := &networkapi.IPv4Address{Slug: f.id("pip"), Name: str("name"), Tags: tags(), Address: "203.0.113.9"}
		f.pips[p.Slug] = p
		out(201, p)
	case r.Method == "GET" && seg[0] == "ipv4-addresses" && len(seg) == 2:
		if f.pips[seg[1]] == nil {
			notFound()
			return
		}
		out(200, f.pips[seg[1]])
	case r.Method == "DELETE" && seg[0] == "ipv4-addresses":
		if f.pips[seg[1]] == nil {
			notFound()
			return
		}
		delete(f.pips, seg[1])
		w.WriteHeader(204)
	case call == "POST /target-groups":
		g := &lbapi.TargetGroup{Slug: f.id("tgp"), Name: str("name"), Tags: tags(), VPC: str("vpc"), Port: num("port"), ProxyProtocol: str("proxyProtocol"), Protocol: str("protocol"),
			DeregistrationDelaySeconds: 300, HealthCheck: lbapi.HealthCheck{Protocol: "tcp", IntervalSeconds: 5, TimeoutSeconds: 2, HealthyThreshold: 3, UnhealthyThreshold: 2}}
		f.groups[g.Slug] = g
		out(201, g)
	case seg[0] == "target-groups" && len(seg) == 2:
		g := f.groups[seg[1]]
		if g == nil {
			notFound()
			return
		}
		switch r.Method {
		case "GET":
			out(200, g)
		case "PATCH":
			if v, ok := body["port"].(float64); ok {
				g.Port = int(v)
			}
			if v, ok := body["proxyProtocol"].(string); ok {
				g.ProxyProtocol = v
			}
			out(200, g)
		case "DELETE":
			for _, l := range f.listeners {
				if l.TargetGroup != nil && *l.TargetGroup == g.Slug {
					out(409, map[string]any{"error": map[string]string{"code": "target_group_in_use", "message": "in use"}})
					return
				}
			}
			delete(f.groups, seg[1])
			w.WriteHeader(204)
		}
	case seg[0] == "target-groups" && len(seg) >= 3:
		if f.groups[seg[1]] == nil {
			notFound()
			return
		}
		switch r.Method {
		case "GET":
			var ts []lbapi.Target
			for _, t := range f.targets {
				if t.TargetGroup == seg[1] {
					ts = append(ts, *t)
				}
			}
			out(200, map[string]any{"targets": ts, "nextCursor": nil})
		case "POST":
			for _, t := range f.targets {
				if t.TargetGroup == seg[1] && t.PrivateIPv4 == str("privateIpv4") && *t.Port == num("port") {
					out(409, map[string]any{"error": map[string]string{"code": "target_already_registered", "message": "registered"}})
					return
				}
			}
			port := num("port")
			t := &lbapi.Target{ID: f.id("tgt"), TargetGroup: seg[1], PrivateIPv4: str("privateIpv4"), Port: &port, State: "initial"}
			f.targets[t.ID] = t
			out(201, t)
		case "DELETE":
			t := f.targets[seg[3]]
			if t == nil {
				notFound()
				return
			}
			t.State = "draining"
			out(200, t)
		}
	case call == "POST /load-balancers":
		id := f.id("uuid")
		v4 := "203.0.113.9"
		lb := &lbapi.LoadBalancer{ID: id, Slug: f.id("lbr"), Name: str("name"), Tags: tags(), Subnet: str("subnet"), Status: f.lbStatus, IPv4Address: &v4}
		f.lbs[id], f.lbs[lb.Slug] = lb, lb
		op := &lbapi.Operation{ID: f.id("op"), Status: "running"}
		op.Target.ID = id
		f.ops[op.ID] = op
		out(202, op)
	case seg[0] == "load-balancers" && len(seg) == 2:
		lb := f.lbs[seg[1]]
		if lb == nil {
			notFound()
			return
		}
		switch r.Method {
		case "GET":
			out(200, lb)
		case "DELETE":
			lb.Status = "deleting"
			for id, l := range f.listeners {
				if l.LoadBalancer == lb.Slug {
					delete(f.listeners, id)
				}
			}
			out(202, lbapi.Operation{ID: f.id("op"), Status: "running"})
		}
	case seg[0] == "load-balancers" && len(seg) >= 3:
		lb := f.lbs[seg[1]]
		if lb == nil {
			notFound()
			return
		}
		switch r.Method {
		case "GET":
			var ls []lbapi.Listener
			for _, l := range f.listeners {
				if l.LoadBalancer == lb.Slug {
					ls = append(ls, *l)
				}
			}
			out(200, map[string]any{"listeners": ls, "nextCursor": nil})
		case "POST":
			tg := str("targetGroup")
			l := &lbapi.Listener{ID: f.id("lsn"), LoadBalancer: lb.Slug, Protocol: str("protocol"), Port: num("port"), TargetGroup: &tg, IdleTimeoutSeconds: 350,
				AllowedSourceCidrs: []string{"0.0.0.0/0"}}
			if c := str("certificate"); c != "" {
				l.Certificate = &c
			}
			if more, ok := body["additionalCertificates"].([]any); ok {
				for _, m := range more {
					l.AdditionalCertificates = append(l.AdditionalCertificates, m.(string))
				}
			}
			f.listeners[l.ID] = l
			out(201, l)
		case "PATCH":
			l := f.listeners[seg[3]]
			if v, ok := body["protocol"].(string); ok {
				l.Protocol = v
			}
			if v, ok := body["targetGroup"].(string); ok {
				l.TargetGroup = &v
			}
			out(200, l)
		case "DELETE":
			delete(f.listeners, seg[3])
			w.WriteHeader(204)
		}
	default:
		notFound()
	}
}

// keyOf is the last Idempotency-Key sent with call.
func (f *fakeEZGH) keyOf(call string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.writes) - 1; i >= 0; i-- {
		if f.writes[i] == call {
			return f.keys[i]
		}
	}
	return ""
}
