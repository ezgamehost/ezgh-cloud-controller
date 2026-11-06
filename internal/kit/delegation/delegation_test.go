package delegation_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/authz"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/delegation"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/vectors"
)

type wireReference struct {
	V         int    `json:"v"`
	Org       string `json:"org"`
	Principal struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	} `json:"principal"`
	Credential struct {
		Kind string `json:"kind"`
		ID   string `json:"id"`
	} `json:"credential"`
	At    int64    `json:"at"`
	Chain []string `json:"chain"`
}

func (w wireReference) reference() delegation.Reference {
	return delegation.Reference{
		Org: w.Org, PrincipalType: w.Principal.Type, PrincipalID: w.Principal.ID,
		CredentialKind: w.Credential.Kind, CredentialID: w.Credential.ID,
		At: time.Unix(w.At, 0).UTC(), Chain: w.Chain,
	}
}

func equal(a, b delegation.Reference) bool {
	return a.Org == b.Org && a.PrincipalType == b.PrincipalType && a.PrincipalID == b.PrincipalID &&
		a.CredentialKind == b.CredentialKind && a.CredentialID == b.CredentialID && a.At.Equal(b.At) &&
		strings.Join(a.Chain, ",") == strings.Join(b.Chain, ",")
}

// contracts/vectors/caller-reference.json, both ways: the formatter writes each header byte for
// byte, the parser reads it back, and every invalid header is refused with its message.
func TestReferenceVectors(t *testing.T) {
	var file struct {
		Valid []struct {
			Name      string        `json:"name"`
			Reference wireReference `json:"reference"`
			Header    string        `json:"header"`
		} `json:"valid"`
		Invalid []struct {
			Name           string          `json:"name"`
			Header         json.RawMessage `json:"header"`
			Message        string          `json:"message"`
			ClassifierOnly bool            `json:"classifierOnly"`
		} `json:"invalid"`
	}
	vectors.Load(t, "caller-reference.json", &file)
	if len(file.Valid) < 8 || len(file.Invalid) < 30 {
		t.Fatalf("only %d valid and %d invalid cases", len(file.Valid), len(file.Invalid))
	}
	for _, c := range file.Valid {
		want := c.Reference.reference()
		if got := want.String(); got != c.Header {
			t.Errorf("%s: String() = %q, want %q", c.Name, got, c.Header)
		}
		got, err := delegation.Parse(c.Header)
		if err != nil || !equal(got, want) {
			t.Errorf("%s: Parse = %+v, %v", c.Name, got, err)
		}
		if _, err := delegation.FromHeaders([]string{c.Header}); err != nil {
			t.Errorf("%s: FromHeaders: %v", c.Name, err)
		}
	}
	for _, c := range file.Invalid {
		var err error
		if c.ClassifierOnly {
			var values []string
			if e := json.Unmarshal(c.Header, &values); e != nil {
				t.Fatal(e)
			}
			_, err = delegation.FromHeaders(values)
		} else {
			var header string
			if e := json.Unmarshal(c.Header, &header); e != nil {
				t.Fatal(e)
			}
			_, err = delegation.Parse(header)
			if _, again := delegation.FromHeaders([]string{header}); again == nil || again.Error() != c.Message {
				t.Errorf("%s: FromHeaders: %v", c.Name, again)
			}
		}
		if err == nil || err.Error() != c.Message {
			t.Errorf("%s: %v, want %q", c.Name, err, c.Message)
		}
	}
}

func answer(kind, id string) *authz.BatchAnswer {
	a := &authz.BatchAnswer{Organization: authz.Organization{ID: "0199a3c2-5b1e-7d40-9f3a-2c8e6b1d4f70"}, Principal: authz.Principal{Type: "user", ID: "jtMJXwL0R8aB3cD4eF5gH6iJ"}}
	if kind != "" {
		a.Credential = &authz.CredentialRef{Kind: kind, ID: id}
	}
	return a
}

// FromAnswer names each kind as the answer does, with at in whole seconds and the edge's
// namespace; without a credential it's ErrNotDelegable.
func TestFromAnswer(t *testing.T) {
	at := time.Unix(1792051200, 412_000_000)
	for _, kind := range delegation.CredentialKinds {
		r, err := delegation.FromAnswer(answer(kind, "0199a3e8-4c5d-7e6f-8a9b-0c1d2e3f4a5b"), at, "compute")
		if err != nil {
			t.Fatal(kind, err)
		}
		want := "v=1;org=0199a3c2-5b1e-7d40-9f3a-2c8e6b1d4f70;principal=user:jtMJXwL0R8aB3cD4eF5gH6iJ;credential=" + kind + ":0199a3e8-4c5d-7e6f-8a9b-0c1d2e3f4a5b;at=1792051200;chain=compute"
		if r.String() != want || r.Caller() != "compute" || r.Origin() != "compute" {
			t.Errorf("%s: %s", kind, r)
		}
	}
	if _, err := delegation.FromAnswer(answer("", ""), at, "compute"); !errors.Is(err, delegation.ErrNotDelegable) {
		t.Errorf("no credential: %v", err)
	}
	if _, err := delegation.FromAnswer(nil, at, "compute"); !errors.Is(err, delegation.ErrNotDelegable) {
		t.Errorf("nil: %v", err)
	}
	if _, err := delegation.FromAnswer(answer("apiKey", "ezgh_0123456789abcdefghijABCDEFGHIJklmnopqrst"), at, "compute"); !errors.Is(err, delegation.ErrSecret) {
		t.Errorf("a secret-shaped ID: %v", err)
	}
}

// Relay appends the receiver's namespace once: never a third entry or a repeat.
func TestRelay(t *testing.T) {
	r, err := delegation.FromAnswer(answer("session", "Qm7cXw2KpL9sTn4vBd8yRf3h"), time.Unix(1792051200, 0), "compute")
	if err != nil {
		t.Fatal(err)
	}
	relayed, err := r.Relay("blockstore")
	if err != nil || relayed.Caller() != "blockstore" || relayed.Origin() != "compute" || !strings.HasSuffix(relayed.String(), ";chain=compute,blockstore") {
		t.Fatalf("Relay = %s, %v", relayed, err)
	}
	if len(r.Chain) != 1 {
		t.Error("Relay changed the original")
	}
	if _, err := relayed.Relay("images"); !errors.Is(err, delegation.ErrChain) {
		t.Errorf("third entry: %v", err)
	}
	if _, err := r.Relay("compute"); !errors.Is(err, delegation.ErrChain) {
		t.Errorf("repeat: %v", err)
	}
}

// Do sends the receiver credential, the reference, X-Forwarded-For and X-Request-Id, and never a
// customer credential; without a reference it makes no call.
func TestClientDo(t *testing.T) {
	var got http.Header
	var path string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, path = r.Header.Clone(), r.URL.RequestURI()
	}))
	defer server.Close()
	r, _ := delegation.FromAnswer(answer("oauth", "0199a3e8-4c5d-7e6f-8a9b-0c1d2e3f4a5b"), time.Unix(1792051200, 0), "compute")
	client := &delegation.Client{Receiver: "images", BaseURL: server.URL, Credential: "imk_0123456789abcdef.AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"}
	req, _ := http.NewRequest(http.MethodGet, "/internal/v1/projects/p/image-resolution?ref=linux%2Fubuntu", nil)
	req.Header.Set("Authorization", "Bearer ezgh_at_0123456789abcdefghijABCDEFGHIJklmnopqrstuvw")
	req.Header.Set("Cookie", "better-auth.session_token=abc.def")
	req.Header.Set("X-Ezgh-Caller", "forged")
	resp, err := client.Do(context.Background(), &delegation.Outbound{Reference: r, ForwardedFor: "203.0.113.9, 198.51.100.23", RequestID: "req-1"}, req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if path != "/internal/v1/projects/p/image-resolution?ref=linux%2Fubuntu" {
		t.Errorf("path %s", path)
	}
	if got.Get("Authorization") != "Bearer "+client.Credential || got.Get("Cookie") != "" || got.Values("X-Ezgh-Caller")[0] != r.String() || len(got.Values("X-Ezgh-Caller")) != 1 ||
		got.Get("X-Forwarded-For") != "203.0.113.9, 198.51.100.23" || got.Get("X-Request-Id") != "req-1" {
		t.Errorf("headers %v", got)
	}
	got = nil
	if _, err := client.Do(context.Background(), nil, req); err == nil || got != nil {
		t.Errorf("nil outbound: %v, called %t", err, got != nil)
	}
	if _, err := client.Do(context.Background(), &delegation.Outbound{}, req); err == nil || got != nil {
		t.Errorf("empty reference: %v, called %t", err, got != nil)
	}
}

// contracts/vectors/delegated-checks.json edgeMapping.
func TestCustomerErrorVectors(t *testing.T) {
	var file struct {
		EdgeMapping []struct {
			Name     string `json:"name"`
			Receiver struct {
				Status     *int   `json:"status"`
				Code       string `json:"code"`
				Message    string `json:"message"`
				RetryAfter int    `json:"retryAfter"`
			} `json:"receiver"`
			Expected *struct {
				Status     int    `json:"status"`
				Code       string `json:"code"`
				Message    string `json:"message"`
				RetryAfter int    `json:"retryAfter"`
			} `json:"expected"`
		} `json:"edgeMapping"`
	}
	vectors.Load(t, "delegated-checks.json", &file)
	for _, c := range file.EdgeMapping {
		status := 0
		if c.Receiver.Status != nil {
			status = *c.Receiver.Status
		}
		e, ok := delegation.CustomerError(status, c.Receiver.Code, c.Receiver.Message, c.Receiver.RetryAfter)
		if ok != (c.Expected != nil) {
			t.Errorf("%s: ok %t", c.Name, ok)
			continue
		}
		if !ok {
			continue
		}
		if e.Status != c.Expected.Status || e.Code != c.Expected.Code || e.RetryAfter != c.Expected.RetryAfter || (c.Expected.Message != "" && e.Message != c.Expected.Message) {
			t.Errorf("%s: %d %s %q Retry-After %d", c.Name, e.Status, e.Code, e.Message, e.RetryAfter)
		}
	}
}

// ResponseError reads the receiver's body and Retry-After.
func TestResponseError(t *testing.T) {
	rec := httptest.NewRecorder()
	rec.Header().Set("Retry-After", "3")
	rec.WriteHeader(http.StatusTooManyRequests)
	_, _ = rec.WriteString(`{"error":{"code":"too_many_requests","message":"slow down"}}`)
	e, ok, code, _ := delegation.ResponseError(rec.Result())
	if !ok || e.Status != http.StatusTooManyRequests || e.RetryAfter != 3 || code != "too_many_requests" {
		t.Errorf("%v %t %s", e, ok, code)
	}
}

// Do sends only to BaseURL's scheme and host, only allowlisted headers (whatever their spelling in
// the map), and follows no redirect.
func TestClientDoHardening(t *testing.T) {
	var elsewhere, receiver int
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { elsewhere++ }))
	defer other.Close()
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receiver++
		got = r.Header.Clone()
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, other.URL+"/stolen", http.StatusFound)
		}
	}))
	defer server.Close()
	r, _ := delegation.FromAnswer(answer("apiKey", "0199a3c4-1e2f-7a31-9b4c-5d6e7f809102"), time.Unix(1792051200, 0), "compute")
	out := &delegation.Outbound{Reference: r}
	client := &delegation.Client{Receiver: "images", BaseURL: server.URL, Credential: "imk_0123456789abcdef.AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8", HTTP: &http.Client{}}

	req, _ := http.NewRequest(http.MethodGet, other.URL+"/x", nil)
	if _, err := client.Do(context.Background(), out, req); err == nil || elsewhere != 0 {
		t.Errorf("another host: %v, %d calls there", err, elsewhere)
	}
	req, _ = http.NewRequest(http.MethodGet, strings.Replace(server.URL, "http://", "https://", 1)+"/x", nil)
	if _, err := client.Do(context.Background(), out, req); err == nil || receiver != 0 {
		t.Errorf("another scheme: %v", err)
	}

	req, _ = http.NewRequest(http.MethodPost, server.URL+"/x", strings.NewReader("{}"))
	req.Header["cookie"] = []string{"better-auth.session_token=abc.def"}
	req.Header["authorization"] = []string{"Bearer ezgh_at_0123456789abcdefghijABCDEFGHIJklmnopqrstuvw"}
	req.Header["X-Custom"] = []string{"x"}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "k1")
	resp, err := client.Do(context.Background(), out, req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if len(got.Values("Authorization")) != 1 || got.Get("Authorization") != "Bearer "+client.Credential || got.Get("Cookie") != "" || got.Get("X-Custom") != "" ||
		got.Get("Content-Type") != "application/json" || got.Get("Idempotency-Key") != "k1" {
		t.Errorf("headers %v", got)
	}

	req, _ = http.NewRequest(http.MethodGet, "/redirect", nil)
	resp, err = client.Do(context.Background(), out, req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound || elsewhere != 0 {
		t.Errorf("redirect: %d, %d calls elsewhere", resp.StatusCode, elsewhere)
	}
}

// Do keeps the request path's escaping when joining it onto BaseURL (an ID's %2F stays encoded),
// and refuses a path with a dot-segment once decoded.
func TestClientDoEscapedPath(t *testing.T) {
	var got []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.EscapedPath())
	}))
	defer server.Close()
	r, _ := delegation.FromAnswer(answer("apiKey", "0199a3c4-1e2f-7a31-9b4c-5d6e7f809102"), time.Unix(1792051200, 0), "compute")
	out := &delegation.Outbound{Reference: r}
	client := &delegation.Client{Receiver: "images", BaseURL: server.URL + "/base/", Credential: "imk_0123456789abcdef.AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"}
	do := func(target string) error {
		u, err := url.Parse(target)
		if err != nil {
			t.Fatal(err)
		}
		req := &http.Request{Method: http.MethodGet, URL: u, Header: http.Header{}}
		resp, err := client.Do(context.Background(), out, req)
		if err == nil {
			_ = resp.Body.Close()
		}
		return err
	}
	if err := do("/internal/v1/images/linux%2Fubuntu/content"); err != nil {
		t.Fatal(err)
	}
	if err := do(server.URL + "/internal/v1/images/a%2Fb"); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "/base/internal/v1/images/linux%2Fubuntu/content" || got[1] != "/internal/v1/images/a%2Fb" {
		t.Errorf("paths %v", got)
	}
	for _, bad := range []string{"/internal/v1/images/a%2F..%2Fb", "/internal/v1/../admin", "/internal/v1/images/%2E%2E/x", "/internal/./v1", server.URL + "/internal/v1/a%2F..%2Fb"} {
		if err := do(bad); err == nil {
			t.Errorf("%s was sent", bad)
		}
	}
	if len(got) != 2 {
		t.Errorf("%d calls", len(got))
	}
}
