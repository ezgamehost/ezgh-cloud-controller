package service_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/manifest"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/service"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/testdb"
)

// A PKI-host endpoint is served by the product's own PKI servers (manifest.CheckPKIRoutes), never
// by Service: Handler builds without a handler for it, never routes it, and refuses one.
func TestPKIEndpointsAreNotServedHere(t *testing.T) {
	db := testdb.New(t)
	m := testManifest()
	m.PKI = &manifest.PKIHost{Hostname: manifest.PKIHostname, Port: 4705}
	m.Endpoints = append(m.Endpoints,
		manifest.Endpoint{Method: "GET", Path: "/crl/:file", Host: manifest.HostPKI, Auth: "none"},
		manifest.Endpoint{Method: "POST", Path: "/acme/:caId/new-order", Host: manifest.HostPKI, Auth: "none"},
	)
	build := func(withPKIHandler bool) (http.Handler, error) {
		s := service.New(service.Options{Manifest: m, DB: db.DB, Logger: testdb.Logger(), OpenAPIOverlay: stubOverlay(m)})
		s.Handle("GET", "/v1/organizations/:orgId/things", func(*service.Call) error { return nil })
		s.Handle("POST", "/v1/organizations/:orgId/things", func(*service.Call) error { return nil })
		s.Handle("GET", "/internal/v1/things", func(*service.Call) error { return nil })
		if withPKIHandler {
			s.Handle("GET", "/crl/:file", func(*service.Call) error { return nil })
		}
		return s.Handler()
	}
	handler, err := build(false)
	if err != nil {
		t.Fatalf("a PKI endpoint needed a handler: %v", err)
	}
	for _, r := range []*http.Request{httptest.NewRequest("GET", "/crl/x.crl", nil), httptest.NewRequest("POST", "/acme/x/new-order", nil)} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, r)
		if rec.Code != 404 {
			t.Errorf("the API host served the PKI endpoint %s %s: %d", r.Method, r.URL.Path, rec.Code)
		}
	}
	if _, err := build(true); err == nil || !strings.Contains(err.Error(), "GET /crl/:file is a PKI-host endpoint: the product's PKI server serves it, not this API") {
		t.Fatalf("got %v", err)
	}
}
