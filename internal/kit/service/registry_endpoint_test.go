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

// A registry-host endpoint is served by the product's own OCI server
// (manifest.CheckRegistryRoutes), never by Service: Handler builds without a handler for it,
// never routes it, and refuses one.
func TestRegistryEndpointsAreNotServedHere(t *testing.T) {
	db := testdb.New(t)
	m := testManifest()
	m.Registry = &manifest.RegistryHost{Domain: "registry.ezghcloud.com", Port: 4701}
	m.Endpoints = append(m.Endpoints,
		manifest.Endpoint{Method: "GET", Path: "/v2/", Host: manifest.HostRegistry, Auth: "none"},
		manifest.Endpoint{Method: "HEAD", Path: "/v2/<name>/manifests/<reference>", Host: manifest.HostRegistry, Auth: manifest.AuthRegistry, Action: "widgets.images.pull"},
	)
	build := func(withRegistryHandler bool) (http.Handler, error) {
		s := service.New(service.Options{Manifest: m, DB: db.DB, Logger: testdb.Logger(), OpenAPIOverlay: stubOverlay(m)})
		s.Handle("GET", "/v1/organizations/:orgId/things", func(*service.Call) error { return nil })
		s.Handle("POST", "/v1/organizations/:orgId/things", func(*service.Call) error { return nil })
		s.Handle("GET", "/internal/v1/things", func(*service.Call) error { return nil })
		if withRegistryHandler {
			s.Handle("GET", "/v2/", func(*service.Call) error { return nil })
		}
		return s.Handler()
	}
	handler, err := build(false)
	if err != nil {
		t.Fatalf("a registry endpoint needed a handler: %v", err)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/v2/", nil))
	if rec.Code != 404 {
		t.Errorf("the API host served the registry endpoint: %d", rec.Code)
	}
	if _, err := build(true); err == nil || !strings.Contains(err.Error(), "GET /v2/ is a registry-host endpoint: the registry server serves it, not this API") {
		t.Fatalf("got %v", err)
	}
}
