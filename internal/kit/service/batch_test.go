package service_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/authz"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/httpapi"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/manifest"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/service"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/testdb"
)

// Call.CheckBatch (contracts/identity-and-authz.md, "Batch checks"; decision D13): one org-api
// call for several resources, attributing the call as Check does, with the edge host's public
// base in requestUrl.

const okCredential = "ok_0123456789abcdef.AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"

type fakeBatch struct {
	mu     sync.Mutex
	bodies []map[string]any
	auth   []string
	status int
}

func (f *fakeBatch) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	f.mu.Lock()
	f.bodies = append(f.bodies, body)
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	status := f.status
	f.mu.Unlock()
	if r.URL.Path != "/internal/v1/permissions/batch" {
		w.WriteHeader(404)
		return
	}
	if status != 0 {
		w.WriteHeader(status)
		return
	}
	var results []map[string]any
	for _, c := range body["checks"].([]any) {
		check := c.(map[string]any)
		results = append(results, map[string]any{"action": check["action"], "resource": check["resource"], "allowed": !strings.Contains(check["resource"].(string), "denied")})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"organization": map[string]any{"id": edgeOrgID, "name": "Acme", "slug": orgSlug, "resourceName": "ezgh::" + orgSlug, "isRoot": false},
		"principal":    map[string]any{"type": "ssoUser", "id": "0199a3f1-1a2b-7c3d-8e4f-5a6b7c8d9e0f"},
		"apiKeyId":     nil,
		"userIdentity": json.RawMessage(identity),
		"results":      results,
	})
}

type batchSeen struct {
	answer       *authz.BatchAnswer
	err          *httpapi.Error
	attributable bool
	principal    authz.Principal
	credential   authz.Credential
}

func batchService(t *testing.T, org *fakeBatch) (http.Handler, *[]batchSeen) {
	t.Helper()
	server := httptest.NewServer(org)
	t.Cleanup(server.Close)
	m := edgeManifest()
	s := service.New(service.Options{
		Manifest: m, OrgAPI: &authz.Client{BaseURL: server.URL, PublicBaseURL: "https://widgets.ezghcloud.com", ServiceCredential: okCredential},
		Logger: testdb.Logger(), OpenAPIOverlay: stubOverlay(m), OrgResolver: (&resolver{}).resolve,
	})
	var seen []batchSeen
	check := func(c *service.Call) error {
		checks := []authz.BatchCheck{
			{Action: "widgets.gateways.connect", Resource: "ezgh:us-west-1:" + orgSlug + ":prj_g7h8i9j0k1l2:mgw_4k2m9x0a7qpe"},
			{Action: "widgets.tools.call", Resource: "ezgh:us-west-1:" + orgSlug + ":prj_g7h8i9j0k1l2:mcs_8a1b2c3d4e5f/tools/denied"},
		}
		answer, err := c.CheckBatch(checks)
		p := batchSeen{answer: answer, attributable: c.Attributable(), principal: c.Principal(), credential: c.Credential()}
		if err != nil {
			p.err = httpapi.ErrorOf(err)
		}
		seen = append(seen, p)
		if err != nil {
			return err
		}
		c.JSON(200, map[string]any{"ok": true})
		return nil
	}
	s.Handle("GET", "/central", check)
	s.Handle("GET", "/v1/organizations/:orgId/things", check)
	for _, r := range []string{"POST /v1/organizations/:orgId/things", "GET /internal/v1/things", "POST /central/:gatewaySlug"} {
		method, path, _ := strings.Cut(r, " ")
		s.Handle(method, path, func(*service.Call) error { return nil })
	}
	h, err := s.Handler(manifest.ComponentAPI)
	if err != nil {
		t.Fatal(err)
	}
	return h, &seen
}

func TestCallCheckBatchOnTheEdgeHost(t *testing.T) {
	org := &fakeBatch{}
	h, seen := batchService(t, org)
	rec := edgeDo(h, "GET", "acme.mcp.ezgh.app", "/central?x=1", map[string]string{"Authorization": "Bearer " + ssoToken, "X-Forwarded-For": "203.0.113.9"})
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	body := org.bodies[0]
	if body["organizationId"] != edgeOrgID || body["requestUrl"] != "https://acme.mcp.ezgh.app/central" || body["credential"].(map[string]any)["authorization"] != "Bearer "+ssoToken {
		t.Errorf("body %v", body)
	}
	if org.auth[0] != "Bearer "+okCredential {
		t.Errorf("Authorization %q", org.auth[0])
	}
	p := (*seen)[0]
	if !p.attributable || p.principal.Type != "ssoUser" || len(p.answer.Results) != 2 || !p.answer.Results[0].Allowed() || p.answer.Results[1].Allowed() {
		t.Errorf("%+v", p)
	}
	if p.credential.PublicBase != "https://acme.mcp.ezgh.app" || !p.credential.IsSSO() {
		t.Errorf("credential %+v", p.credential)
	}
}

func TestCallCheckBatchOnTheAPIHost(t *testing.T) {
	org := &fakeBatch{}
	h, seen := batchService(t, org)
	if rec := edgeDo(h, "GET", "widgets.ezghcloud.com", "/v1/organizations/"+edgeOrgID+"/things", map[string]string{"Authorization": "Bearer " + apiKey}); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if _, sent := org.bodies[0]["requestUrl"]; sent || org.bodies[0]["organizationId"] != edgeOrgID {
		t.Errorf("an API key: %v", org.bodies[0])
	}
	if p := (*seen)[0]; p.credential.PublicBase != "" || !p.attributable {
		t.Errorf("%+v", p)
	}
}

// org-api's refusals are the call's, and a refused call isn't attributable.
func TestCallCheckBatchRefusals(t *testing.T) {
	for status, want := range map[int]int{401: 401, 404: 404, 503: 503, 429: 429} {
		org := &fakeBatch{status: status}
		h, seen := batchService(t, org)
		rec := edgeDo(h, "GET", "acme.mcp.ezgh.app", "/central", map[string]string{"Authorization": "Bearer " + ssoToken})
		if rec.Code != want || (*seen)[0].attributable {
			t.Errorf("org-api %d: %d %+v", status, rec.Code, (*seen)[0])
		}
	}
}
