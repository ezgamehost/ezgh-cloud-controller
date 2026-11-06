package l7

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/cloud/config"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/ezghclient/lbapi"
)

var (
	ctx     = context.Background()
	testCfg = config.Config{Cluster: "kcl_a", ClusterUUID: "0199a3c2-0000-7000-8000-00000000c1a5", Org: "org_a", Project: "prj_a", VPC: "vpc_a", Subnet: "sbn_a"}
)

type apiKey string

func (k apiKey) APIKey(context.Context) (string, error) { return string(k), nil }

// fakeEZGH is lb-api, network-api's IPv4 addresses and certificates-api's imports in memory.
// Writes are logged as "METHOD /path" below the project, with each one's body, Idempotency-Key and
// If-Match.
type fakeEZGH struct {
	mu        sync.Mutex
	n         int
	lbs       map[string]map[string]any // by ID and by slug
	listeners map[string]*lbapi.L7Listener
	versions  map[string]int
	owner     map[string]string // listener → lbr_
	groups    map[string]map[string]any
	certs     map[string]map[string]any
	pips      map[string]map[string]any
	writes    []string
	bodies    []map[string]any
	keys      []string
	ifMatch   []string
	// fail answers the first request whose "METHOD /path" starts with a key with that status
	// and code.
	fail     map[string][2]string
	lbStatus string
}

func newFake(t *testing.T) (*fakeEZGH, *ezghclient.Clients) {
	f := &fakeEZGH{lbs: map[string]map[string]any{}, listeners: map[string]*lbapi.L7Listener{}, versions: map[string]int{}, owner: map[string]string{},
		groups: map[string]map[string]any{}, certs: map[string]map[string]any{}, pips: map[string]map[string]any{}, fail: map[string][2]string{}, lbStatus: "active"}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, ezghclient.NewAt(apiKey("ezgh_test"), "test", func(string) string { return srv.URL })
}

func (f *fakeEZGH) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.writes...)
}

func (f *fakeEZGH) reset() {
	f.mu.Lock()
	f.writes, f.bodies, f.keys, f.ifMatch = nil, nil, nil, nil
	f.mu.Unlock()
}

// body is the last body sent with a write starting with call.
func (f *fakeEZGH) body(call string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.writes) - 1; i >= 0; i-- {
		if strings.HasPrefix(f.writes[i], call) {
			return f.bodies[i]
		}
	}
	return nil
}

func (f *fakeEZGH) id(prefix string) string { f.n++; return fmt.Sprintf("%s_%d", prefix, f.n) }

func (f *fakeEZGH) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/v1/organizations/org_a/projects/prj_a")
	call := r.Method + " " + path
	var raw json.RawMessage
	_ = json.NewDecoder(r.Body).Decode(&raw)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	out := func(status int, v any) { w.WriteHeader(status); _ = json.NewEncoder(w).Encode(v) }
	refuse := func(status int, code, msg string) {
		out(status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
	}
	if r.Method != http.MethodGet {
		f.writes = append(f.writes, call)
		f.bodies = append(f.bodies, body)
		f.keys = append(f.keys, r.Header.Get("Idempotency-Key"))
		f.ifMatch = append(f.ifMatch, r.Header.Get("If-Match"))
	}
	for prefix, v := range f.fail {
		if strings.HasPrefix(call, prefix) {
			delete(f.fail, prefix)
			msg := "refused"
			if v[1] == "access_denied" {
				msg = "This API key isn't allowed to lb.loadBalancers.create"
			}
			var status int
			_, _ = fmt.Sscan(v[0], &status)
			refuse(status, v[1], msg)
			return
		}
	}
	seg := strings.Split(strings.Trim(path, "/"), "/")
	str := func(k string) string { s, _ := body[k].(string); return s }
	switch {
	case strings.HasPrefix(call, "GET /v1/organizations/org_a/operations/"):
		out(200, map[string]any{"id": seg[len(seg)-1], "status": "running"})
	case call == "GET /load-balancers":
		var lbs []map[string]any
		for k, lb := range f.lbs {
			if k == lb["slug"] {
				lbs = append(lbs, lb)
			}
		}
		out(200, map[string]any{"loadBalancers": lbs, "nextCursor": nil})
	case call == "GET /target-groups":
		var gs []map[string]any
		for _, g := range f.groups {
			gs = append(gs, g)
		}
		out(200, map[string]any{"targetGroups": gs, "nextCursor": nil})
	case call == "GET /ipv4-addresses":
		out(200, map[string]any{"ipv4Addresses": []any{}, "nextCursor": nil})
	case call == "GET /certificates":
		var cs []map[string]any
		for _, c := range f.certs {
			cs = append(cs, c)
		}
		out(200, map[string]any{"certificates": cs, "nextCursor": nil})
	case r.Method == "GET" && seg[0] == "certificates" && len(seg) == 2:
		if f.certs[seg[1]] == nil {
			refuse(404, "not_found", "not found")
			return
		}
		out(200, f.certs[seg[1]])
	case call == "POST /ipv4-addresses":
		p := map[string]any{"slug": f.id("pip"), "name": str("name"), "tags": body["tags"], "address": "203.0.113.9"}
		f.pips[p["slug"].(string)] = p
		out(201, p)
	case seg[0] == "ipv4-addresses" && len(seg) == 2:
		if f.pips[seg[1]] == nil {
			refuse(404, "not_found", "not found")
			return
		}
		if r.Method == "DELETE" {
			delete(f.pips, seg[1])
			w.WriteHeader(204)
			return
		}
		out(200, f.pips[seg[1]])
	case call == "POST /certificates/import":
		id := f.id("crt")
		f.certs[id] = map[string]any{"slug": id, "name": str("name"), "tags": body["tags"]}
		out(201, f.certs[id])
	case r.Method == "POST" && seg[0] == "certificates" && len(seg) == 3:
		if f.certs[seg[1]] == nil {
			refuse(404, "not_found", "not found")
			return
		}
		out(200, f.certs[seg[1]])
	case r.Method == "DELETE" && seg[0] == "certificates":
		if f.certs[seg[1]] == nil {
			refuse(404, "not_found", "not found")
			return
		}
		delete(f.certs, seg[1])
		out(202, map[string]any{"id": f.id("op"), "status": "running"})
	case call == "POST /target-groups":
		g := map[string]any{"slug": f.id("tgp"), "name": str("name"), "tags": body["tags"], "protocol": str("protocol"), "port": body["port"], "proxyProtocol": str("proxyProtocol"),
			"deregistrationDelaySeconds": 300, "healthCheck": map[string]any{"protocol": "http"}}
		f.groups[g["slug"].(string)] = g
		out(201, g)
	case seg[0] == "target-groups" && len(seg) == 2:
		g := f.groups[seg[1]]
		if g == nil {
			refuse(404, "not_found", "not found")
			return
		}
		switch r.Method {
		case "GET":
			out(200, g)
		case "DELETE":
			for _, l := range f.listeners {
				if b, _ := json.Marshal(l); strings.Contains(string(b), `"`+seg[1]+`"`) {
					refuse(409, "target_group_in_use", "in use")
					return
				}
			}
			delete(f.groups, seg[1])
			w.WriteHeader(204)
		default:
			out(200, g)
		}
	case call == "POST /load-balancers":
		id := f.id("uuid")
		lb := map[string]any{"id": id, "slug": f.id("lbr"), "name": str("name"), "tags": body["tags"], "subnet": str("subnet"), "status": f.lbStatus,
			"ipv4Address": "203.0.113.9", "ipv6Address": "2001:db8::9"}
		f.lbs[id], f.lbs[lb["slug"].(string)] = lb, lb
		out(202, map[string]any{"id": f.id("op"), "status": "running", "target": map[string]any{"id": id}})
	case seg[0] == "load-balancers" && len(seg) == 2:
		lb := f.lbs[seg[1]]
		if lb == nil {
			refuse(404, "not_found", "not found")
			return
		}
		switch r.Method {
		case "GET":
			out(200, lb)
		case "DELETE":
			lb["status"] = "deleting"
			for id, owner := range f.owner {
				if owner == lb["slug"] {
					delete(f.listeners, id)
				}
			}
			out(202, map[string]any{"id": f.id("op"), "status": "running"})
		}
	case seg[0] == "load-balancers" && len(seg) >= 3:
		lbr := seg[1]
		switch {
		case r.Method == "GET" && len(seg) == 3:
			var ls []map[string]any
			for id, l := range f.listeners {
				if f.owner[id] == lbr {
					ls = append(ls, map[string]any{"id": id, "protocol": l.Protocol, "port": l.Port})
				}
			}
			out(200, map[string]any{"listeners": ls, "nextCursor": nil})
		case r.Method == "POST":
			var in lbapi.L7ListenerCreate
			_ = json.Unmarshal(raw, &in)
			l := &lbapi.L7Listener{ID: f.id("lsn"), Protocol: in.Protocol, Port: in.Port, DefaultAction: in.DefaultAction, Rules: in.Rules,
				Certificate: in.Certificate, AdditionalCertificates: in.AdditionalCertificates}
			f.listeners[l.ID], f.owner[l.ID], f.versions[l.ID] = l, lbr, 1
			w.Header().Set("ETag", `"1"`)
			out(201, l)
		case r.Method == "GET":
			l := f.listeners[seg[3]]
			if l == nil {
				refuse(404, "not_found", "not found")
				return
			}
			w.Header().Set("ETag", fmt.Sprintf(`"%d"`, f.versions[l.ID]))
			out(200, l)
		case r.Method == "PATCH":
			l := f.listeners[seg[3]]
			if m := r.Header.Get("If-Match"); m != fmt.Sprintf(`"%d"`, f.versions[l.ID]) {
				refuse(412, "precondition_failed", "changed")
				return
			}
			var in lbapi.L7ListenerUpdate
			_ = json.Unmarshal(raw, &in)
			l.DefaultAction, l.Rules = in.DefaultAction, in.Rules
			if in.Protocol != nil {
				l.Protocol = *in.Protocol
			}
			if in.Certificate != nil {
				l.Certificate = in.Certificate
			}
			if in.AdditionalCertificates != nil {
				l.AdditionalCertificates = *in.AdditionalCertificates
			}
			f.versions[l.ID]++
			w.Header().Set("ETag", fmt.Sprintf(`"%d"`, f.versions[l.ID]))
			out(200, l)
		case r.Method == "DELETE":
			delete(f.listeners, seg[3])
			w.WriteHeader(204)
		}
	default:
		refuse(404, "not_found", "not found")
	}
}
