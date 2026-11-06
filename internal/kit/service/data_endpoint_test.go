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

// A data-host endpoint is served by the product's own data server (manifest.CheckDataRoutes),
// never by Service: Handler builds without a handler for it, never routes it, and refuses one.
func TestDataEndpointsAreNotServedHere(t *testing.T) {
	db := testdb.New(t)
	m := testManifest()
	m.API.Hostname = "widgets-api.ezghcloud.com"
	m.Data = &manifest.DataHost{Hostname: "widgets.ezghcloud.com", Port: 4701}
	m.Endpoints = append(m.Endpoints,
		manifest.Endpoint{Method: "POST", Path: "/ofrep/v1/evaluate/flags/:key", Host: manifest.HostData, Auth: manifest.AuthProductKey},
		manifest.Endpoint{Method: "GET", Path: "/v1/sync/snapshot", Host: manifest.HostData, Auth: manifest.AuthProductKey},
	)
	build := func(withDataHandler bool) (http.Handler, error) {
		s := service.New(service.Options{Manifest: m, DB: db.DB, Logger: testdb.Logger(), OpenAPIOverlay: stubOverlay(m)})
		s.Handle("GET", "/v1/organizations/:orgId/things", func(*service.Call) error { return nil })
		s.Handle("POST", "/v1/organizations/:orgId/things", func(*service.Call) error { return nil })
		s.Handle("GET", "/internal/v1/things", func(*service.Call) error { return nil })
		if withDataHandler {
			s.Handle("GET", "/v1/sync/snapshot", func(*service.Call) error { return nil })
		}
		return s.Handler()
	}
	handler, err := build(false)
	if err != nil {
		t.Fatalf("a data endpoint needed a handler: %v", err)
	}
	for _, r := range []*http.Request{httptest.NewRequest("GET", "/v1/sync/snapshot", nil), httptest.NewRequest("POST", "/ofrep/v1/evaluate/flags/beta", nil)} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, r)
		if rec.Code != 404 {
			t.Errorf("the API host served the data endpoint %s %s: %d", r.Method, r.URL.Path, rec.Code)
		}
	}
	if _, err := build(true); err == nil || !strings.Contains(err.Error(), "GET /v1/sync/snapshot is a data-host endpoint: the data server serves it, not this API") {
		t.Fatalf("got %v", err)
	}
}
