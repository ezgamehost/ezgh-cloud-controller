package service_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/authz"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/manifest"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/service"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/testdb"
)

// Long endpoints (contracts/api-conventions.md, "Timeouts"): an endpoint's timeoutSeconds replaces
// the server's write timeout for its requests, and the shutdown drain lets them finish.

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

// slowService answers GET /central (timeoutSeconds 5) and GET /central/:gatewaySlug (the server's
// timeout) after wait, reporting the request context's deadline.
func slowService(t *testing.T, wait time.Duration) http.Handler {
	t.Helper()
	m := edgeManifest()
	for i, e := range m.Endpoints {
		if e.Path == "/central" {
			m.Endpoints[i].TimeoutSeconds = 5
		}
	}
	m.Endpoints = append(m.Endpoints, manifest.Endpoint{Method: "GET", Path: "/central/:gatewaySlug", Host: manifest.HostEdge, Auth: "caller", Action: "widgets.gateways.connect"})
	s := service.New(service.Options{Manifest: m, OrgAPI: &authz.Client{PublicBaseURL: "https://widgets.ezghcloud.com"}, Logger: testdb.Logger(), OpenAPIOverlay: stubOverlay(m), OrgResolver: (&resolver{}).resolve})
	slow := func(c *service.Call) error {
		deadline, ok := c.Ctx.Deadline()
		select {
		case <-time.After(wait):
		case <-c.Ctx.Done():
			return c.Ctx.Err()
		}
		c.W.Header().Set("X-Has-Deadline", map[bool]string{true: "yes", false: "no"}[ok && time.Until(deadline) > 0])
		c.JSON(200, "done")
		return nil
	}
	s.Handle("GET", "/central", slow)
	s.Handle("GET", "/central/:gatewaySlug", slow)
	for _, r := range []string{"GET /v1/organizations/:orgId/things", "POST /v1/organizations/:orgId/things", "GET /internal/v1/things", "POST /central/:gatewaySlug"} {
		method, path, _ := strings.Cut(r, " ")
		s.Handle(method, path, func(*service.Call) error { return nil })
	}
	h, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func get(addr, path string) (*http.Response, string, error) {
	req, _ := http.NewRequest("GET", "http://"+addr+path, nil)
	req.Host = "acme.mcp.ezgh.app"
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	return resp, string(body), err
}

// With a 1-second server write timeout, the endpoint with timeoutSeconds still answers after 1.5
// seconds; the one without is cut off. The long one's context has its deadline.
func TestEndpointTimeoutExtendsTheWriteDeadline(t *testing.T) {
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- service.ServeWith(ctx, addr, slowService(t, 1500*time.Millisecond), testdb.Logger(), service.ServeOptions{WriteTimeout: time.Second})
	}()
	t.Cleanup(func() { cancel(); <-done })
	time.Sleep(100 * time.Millisecond)
	resp, body, err := get(addr, "/central")
	if err != nil || resp.StatusCode != 200 || body != `"done"` || resp.Header.Get("X-Has-Deadline") != "yes" {
		t.Fatalf("the long endpoint: %v %v %q", resp, err, body)
	}
	if _, body, err := get(addr, "/central/support-bots"); err == nil && body == `"done"` {
		t.Fatal("the server's write timeout didn't apply to an endpoint without timeoutSeconds")
	}
}

// At shutdown, a request in flight finishes within the drain; with a drain shorter than it,
// ServeWith returns DeadlineExceeded without waiting for it (the process then exits under it).
func TestShutdownDrain(t *testing.T) {
	run := func(drain time.Duration) (string, error) {
		addr := freeAddr(t)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			done <- service.ServeWith(ctx, addr, slowService(t, 1500*time.Millisecond), testdb.Logger(), service.ServeOptions{ShutdownDrain: drain})
		}()
		time.Sleep(100 * time.Millisecond)
		answered := make(chan string, 1)
		go func() {
			_, body, _ := get(addr, "/central")
			answered <- body
		}()
		time.Sleep(200 * time.Millisecond)
		cancel()
		serveErr := <-done
		return <-answered, serveErr
	}
	if body, err := run(5 * time.Second); body != `"done"` || err != nil {
		t.Errorf("a 5 s drain: %q %v", body, err)
	}
	if body, err := run(300 * time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a 0.3 s drain: %q %v", body, err)
	}
}
