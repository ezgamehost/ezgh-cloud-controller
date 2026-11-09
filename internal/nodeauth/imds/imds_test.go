package imds

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// server is a fake metadata service; handle answers the reads.
type server struct {
	mu       sync.Mutex
	puts     int
	requests []*http.Request
	handle   func(w http.ResponseWriter, r *http.Request)
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests = append(s.requests, r)
	s.mu.Unlock()
	if r.URL.Path == "/ezgh/v1/token" {
		s.mu.Lock()
		s.puts++
		n := s.puts
		s.mu.Unlock()
		if r.Method != http.MethodPut || r.Header.Get("EZGH-Metadata-Token-TTL-Seconds") != "21600" {
			http.Error(w, "bad token request", 400)
			return
		}
		fmt.Fprintf(w, "tok%d", n)
		return
	}
	s.handle(w, r)
}

// newClient points the real transport at ts, as if it were 169.254.169.254.
func newClient(t *testing.T, s *server) *Client {
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	hc := HTTPClient()
	hc.Transport.(*http.Transport).DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if addr != "169.254.169.254:80" {
			return nil, &net.OpError{Op: "dial", Net: network, Err: fmt.Errorf("unexpected %s", addr)}
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp", ts.Listener.Addr().String())
	}
	return &Client{HTTP: hc}
}

func instanceJSON(w http.ResponseWriter, prefixes string) {
	fmt.Fprintf(w, `{"id":"ins_a","projectId":"p-uuid","interfaces":[{"id":"eni_p","kind":"private","deviceIndex":1,"mtu":8900,"delegatedPrefixes":%s}]}`, prefixes)
}

func TestTokenFirst(t *testing.T) {
	s := &server{handle: func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("EZGH-Metadata-Token") != "tok1" {
			http.Error(w, `{"error":{"code":"invalid_token"}}`, http.StatusUnauthorized)
			return
		}
		instanceJSON(w, "[]")
	}}
	c := newClient(t, s)
	if _, err := c.Instance(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Instance(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.puts != 1 || s.requests[0].URL.Path != "/ezgh/v1/token" {
		t.Fatalf("puts %d, first %s", s.puts, s.requests[0].URL.Path)
	}
}

func TestReputOnceOn401(t *testing.T) {
	s := &server{handle: func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("EZGH-Metadata-Token") != "tok2" {
			http.Error(w, `{"error":{"code":"invalid_token"}}`, http.StatusUnauthorized)
			return
		}
		instanceJSON(w, "[]")
	}}
	c := newClient(t, s)
	if _, err := c.Instance(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Never valid: one re-PUT, then the 401 is the answer.
	s.handle = func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"code":"invalid_token"}}`, http.StatusUnauthorized)
	}
	_, err := c.Instance(context.Background())
	if e, ok := err.(*Error); !ok || e.Status != 401 || s.puts != 3 {
		t.Fatalf("err %v, puts %d", err, s.puts)
	}
}

func TestHeaders(t *testing.T) {
	s := &server{handle: func(w http.ResponseWriter, r *http.Request) { instanceJSON(w, "[]") }}
	c := newClient(t, s)
	if _, err := c.Instance(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, r := range s.requests {
		if r.Header.Get("EZGH-Metadata") != "true" || r.Host != "169.254.169.254" || r.Header.Get("X-Forwarded-For") != "" {
			t.Fatalf("%s: host %q headers %v", r.URL.Path, r.Host, r.Header)
		}
	}
	if tr := HTTPClient().Transport.(*http.Transport); tr.Proxy != nil {
		t.Fatal("transport has a proxy")
	}
}

func TestIPv6Fallback(t *testing.T) {
	ts := httptest.NewServer(&server{handle: func(w http.ResponseWriter, r *http.Request) { instanceJSON(w, "[]") }})
	defer ts.Close()
	refused, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := refused.Addr().String()
	refused.Close()
	c := &Client{Base4: "http://" + addr, Base6: ts.URL}
	in, err := c.Instance(context.Background())
	if err != nil || in.ID != "ins_a" {
		t.Fatalf("%v %+v", err, in)
	}
	// Any IPv4 answer is final, a 500 included.
	v4 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "{}", 500) }))
	defer v4.Close()
	c = &Client{Base4: v4.URL, Base6: ts.URL}
	if _, err := c.Instance(context.Background()); err == nil {
		t.Fatal("fell back to IPv6 after an IPv4 answer")
	}
}

func TestNoRedirects(t *testing.T) {
	s := &server{handle: func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/elsewhere", http.StatusFound)
	}}
	if _, err := newClient(t, s).Instance(context.Background()); err == nil {
		t.Fatal("redirect followed")
	}
	if len(s.requests) != 2 {
		t.Fatalf("%d requests", len(s.requests))
	}
}

func TestReadCap1MiB(t *testing.T) {
	s := &server{handle: func(w http.ResponseWriter, r *http.Request) { w.Write(make([]byte, 1<<20+1)) }}
	if _, err := newClient(t, s).UserData(context.Background()); err == nil || !strings.Contains(err.Error(), "1 MiB") {
		t.Fatalf("%v", err)
	}
	s.handle = func(w http.ResponseWriter, r *http.Request) { w.Write(make([]byte, 1<<20)) }
	if b, err := newClient(t, s).UserData(context.Background()); err != nil || len(b) != 1<<20 {
		t.Fatal(err)
	}
}

func TestNullDelegatedPrefixesIsUnknown(t *testing.T) {
	for raw, want := range map[string]int{"null": -1, "[]": 0, `[{"prefix":"10.0.1.16/28","generation":"3"}]`: 1} {
		s := &server{handle: func(w http.ResponseWriter, r *http.Request) { instanceJSON(w, raw) }}
		in, err := newClient(t, s).Instance(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		got := in.Interfaces[0].DelegatedPrefixes
		if (want < 0) != (got == nil) || (got != nil && len(*got) != want) {
			t.Fatalf("%s: %v", raw, got)
		}
	}
}

func TestIdentityAudienceEscaped(t *testing.T) {
	s := &server{handle: func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "audience=kubernetes%3Akcl_x" {
			http.Error(w, r.URL.RawQuery, 400)
			return
		}
		fmt.Fprint(w, `{"token":"a.b.c"}`)
	}}
	tok, err := newClient(t, s).Identity(context.Background(), "kubernetes:kcl_x")
	if err != nil || tok != "a.b.c" {
		t.Fatal(tok, err)
	}
}

func TestCredentials(t *testing.T) {
	s := &server{handle: func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"apiKey":"ezgh_secret","expiresAt":"2026-11-06T10:00:00.000Z","bot":"bot_x"}`)
	}}
	c := newClient(t, s)
	cr, err := c.Credentials(context.Background())
	if err != nil || cr.APIKey != "ezgh_secret" || cr.Bot != "bot_x" || cr.ExpiresAt.IsZero() {
		t.Fatal(cr, err)
	}
	s.handle = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		fmt.Fprint(w, `{"error":{"code":"no_bot","message":"none"}}`)
	}
	if _, err := c.Credentials(context.Background()); err != ErrNoBot {
		t.Fatal(err)
	}
	s.handle = func(w http.ResponseWriter, r *http.Request) { http.Error(w, `{"error":{"code":"not_found"}}`, 404) }
	if _, err := c.UserData(context.Background()); err != ErrNoUserData {
		t.Fatal(err)
	}
}

func TestCredentialsStringRedacts(t *testing.T) {
	c := Credentials{APIKey: "ezgh_secret", Bot: "bot_x", ExpiresAt: time.Now()}
	for _, s := range []string{fmt.Sprint(c), fmt.Sprintf("%v %+v %#v %s", c, c, c, c)} {
		if strings.Contains(s, "ezgh_secret") || !strings.Contains(s, "bot_x") {
			t.Fatal(s)
		}
	}
}

// A 401 for a token another caller already replaced doesn't drop the newer one.
func TestStale401KeepsNewerToken(t *testing.T) {
	var c *Client
	s := &server{}
	s.handle = func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("EZGH-Metadata-Token") == "a" {
			c.mu.Lock()
			c.token, c.tokenEnd = "b", time.Now().Add(time.Hour)
			c.mu.Unlock()
			http.Error(w, `{"error":{"code":"invalid_token"}}`, http.StatusUnauthorized)
			return
		}
		instanceJSON(w, "[]")
	}
	c = newClient(t, s)
	c.token, c.tokenEnd = "a", time.Now().Add(time.Hour)
	if _, err := c.Instance(context.Background()); err != nil {
		t.Fatal(err)
	}
	last := s.requests[len(s.requests)-1]
	if s.puts != 0 || last.Header.Get("EZGH-Metadata-Token") != "b" {
		t.Fatalf("puts %d, retry carried %q", s.puts, last.Header.Get("EZGH-Metadata-Token"))
	}
}
