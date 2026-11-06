package service_test

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/authz"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/httpapi"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/manifest"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/quota"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/service"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/testdb"
)

// Receipt-handle and invocation-token endpoints read no caller credential, so nothing on them may
// ask org-api (or Quotas) as the caller: every such Call method is a 500 internal error, a bug in
// the handler, and org-api is never asked. Authenticate already refuses non-public endpoints.
func TestCredentiallessEndpointsAskNoOne(t *testing.T) {
	calls := map[string]func(c *service.Call) error{
		"Authorize": func(c *service.Call) error { return c.Authorize("widgets.things.list", "") },
		"Check":     func(c *service.Call) error { _, err := c.Check([]string{"widgets.things.list"}, ""); return err },
		"CheckBatch": func(c *service.Call) error {
			_, err := c.CheckBatch([]authz.BatchCheck{{Action: "widgets.things.list"}})
			return err
		},
		"AuthorizeProject":     func(c *service.Call) error { _, err := c.AuthorizeProject("widgets.things.list"); return err },
		"AuthorizeProjectOnly": func(c *service.Call) error { _, err := c.AuthorizeProjectOnly("widgets.things.list"); return err },
		"AuthorizeMissing":     func(c *service.Call) error { return c.AuthorizeMissing("widgets.things.list", "Thing") },
		"CheckQuotas":          func(c *service.Call) error { return c.CheckQuotas() },
		"CheckQuotaItems":      func(c *service.Call) error { return c.CheckQuotaItems([]quota.PlanItem{}) },
	}
	for _, kind := range []struct{ auth, path, header string }{
		{manifest.AuthReceiptHandle, "/v1/receipts/ack", ""},
		{manifest.AuthInvocationToken, "/v1/invocations/checkpoint", "Bearer tok"},
	} {
		for name, call := range calls {
			t.Run(kind.auth+"/"+name, func(t *testing.T) {
				org := &fakeOrgAPI{}
				orgServer := httptest.NewServer(org)
				t.Cleanup(orgServer.Close)
				m := testManifest()
				m.Live = &manifest.LiveHost{Hostname: "widgets-live.ezghcloud.com", Port: 4750}
				m.Endpoints = append(m.Endpoints, manifest.Endpoint{Method: "POST", Path: kind.path, Host: manifest.HostLive, Component: manifest.ComponentLive, Auth: kind.auth})
				s := service.New(service.Options{
					Manifest: m, OrgAPI: &authz.Client{BaseURL: orgServer.URL, InternalToken: "t"},
					Origins: httpapi.Origins{List: []string{goodOrigin}}, Logger: testdb.Logger(), OpenAPIOverlay: stubOverlay(m),
				})
				s.Handle("POST", kind.path, func(c *service.Call) error {
					if err := call(c); err != nil {
						return err
					}
					c.JSON(200, map[string]any{})
					return nil
				})
				h, err := s.Handler(manifest.ComponentLive)
				if err != nil {
					t.Fatal(err)
				}
				req := httptest.NewRequest("POST", kind.path, strings.NewReader(`{}`))
				req.Host = "widgets-live.ezghcloud.com"
				req.Header.Set("Cookie", "better-auth.session_token=member")
				req.Header.Set("Origin", goodOrigin)
				if kind.header != "" {
					req.Header.Set("Authorization", kind.header)
				}
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				var body struct {
					Error struct{ Code string } `json:"error"`
				}
				_ = json.Unmarshal(rec.Body.Bytes(), &body)
				if rec.Code != 500 || body.Error.Code != "internal_error" {
					t.Fatalf("%d %s", rec.Code, rec.Body)
				}
				if org.calls != 0 {
					t.Fatalf("asked org-api %d times", org.calls)
				}
			})
		}
	}
}
