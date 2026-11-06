package authz_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/authz"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/vectors"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/resourceurl"
)

// contracts/vectors/customer-credentials.json: every case, through CustomerCredentialKind, Classify,
// IngestKeyFrom and CredentialFrom (a customer credential is forwarded as it came and wins over
// the cookie; anything else, an ingest key included, is 401 and never the cookie).
func TestCustomerCredentialVectors(t *testing.T) {
	var file struct {
		Cases []struct {
			Name      string  `json:"name"`
			Header    string  `json:"header"`
			Kind      *string `json:"kind"`
			IngestKey bool    `json:"ingestKey"`
		} `json:"cases"`
	}
	vectors.Load(t, "customer-credentials.json", &file)
	if len(file.Cases) < 16 {
		t.Fatalf("only %d cases", len(file.Cases))
	}
	ingestKeys := 0
	for _, c := range file.Cases {
		want := ""
		if c.Kind != nil {
			want = *c.Kind
		}
		if got := authz.CustomerCredentialKind(c.Header); got != want {
			t.Errorf("%s: kind %q, want %q", c.Name, got, want)
		}
		class := want
		if c.IngestKey {
			class = authz.KindIngestKey
			ingestKeys++
		}
		if got := authz.Classify(c.Header); got != class {
			t.Errorf("%s: Classify %q, want %q", c.Name, got, class)
		}
		key, keyErr := authz.IngestKeyFrom([]string{c.Header})
		if c.IngestKey != (keyErr == nil) || (c.IngestKey && "Bearer "+key != c.Header) {
			t.Errorf("%s: IngestKeyFrom %q %v", c.Name, key, keyErr)
		}
		ingestAccepted := c.IngestKey || want == authz.KindAPIKey
		token, isAPIKey, credErr := authz.IngestCredentialFrom([]string{c.Header})
		if ingestAccepted != (credErr == nil) || isAPIKey != (want == authz.KindAPIKey) || (ingestAccepted && "Bearer "+token != c.Header) {
			t.Errorf("%s: IngestCredentialFrom %q %v %v", c.Name, token, isAPIKey, credErr)
		}
		r := httptest.NewRequest("GET", "/", nil)
		r.Header["Authorization"] = []string{c.Header}
		r.Header.Set("Cookie", "better-auth.session_token=abc")
		cred, err := authz.CredentialFrom(r)
		switch want {
		case "":
			if err == nil || err.Status != 401 {
				t.Errorf("%s: accepted as %+v", c.Name, cred)
			} else if c.IngestKey && !strings.Contains(err.Message, "ingest host") {
				t.Errorf("%s: refused without saying where ingest keys go: %q", c.Name, err.Message)
			}
		default:
			if err != nil || cred.Header != "Authorization" || cred.Value != c.Header || cred.IsAPIKey() != (want == authz.KindAPIKey) || cred.IsOAuth() != (want == authz.KindOAuth) || !cred.IsBearer() {
				t.Errorf("%s: %+v %v", c.Name, cred, err)
			}
		}
	}
	if ingestKeys == 0 {
		t.Fatal("no ingest key case")
	}
}

// An ingest host's check: exactly one value, a project ingest key, which comes back without
// "Bearer ". Everything else, product credentials included, is 401.
func TestIngestKeyFrom(t *testing.T) {
	const key = "ezgh_ik_0123456789abcdefghijABCDEFGHIJklmnopqrstuvw"
	if got, err := authz.IngestKeyFrom([]string{"Bearer " + key}); err != nil || got != key {
		t.Fatalf("%q %v", got, err)
	}
	for _, values := range [][]string{
		nil,
		{},
		{"Bearer " + key, "Bearer " + key},
		{"Bearer ezgh_0123456789abcdefghijABCDEFGHIJklmnopqrst"},
		{"Bearer ezgh_at_0123456789abcdefghijABCDEFGHIJklmnopqrstuvw"},
		{"Bearer ezgh_sat_0123456789abcdefghijABCDEFGHIJklmnopqrstuvw"},
		{key},
		{"Bearer pk_3k9d0a1m2x7q4b8c.AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"},
	} {
		if got, err := authz.IngestKeyFrom(values); err == nil || err.Status != 401 || err.Code != "unauthenticated" || got != "" {
			t.Errorf("%q: %q %v", values, got, err)
		}
	}
}

// An ingest host's check with API keys: exactly one value, an ingest key or an API key, which
// comes back without "Bearer " and says which it is. Everything else is 401.
func TestIngestCredentialFrom(t *testing.T) {
	const ingestKey = "ezgh_ik_0123456789abcdefghijABCDEFGHIJklmnopqrstuvw"
	const apiKey = "ezgh_0123456789abcdefghijABCDEFGHIJklmnopqrst"
	if got, isAPIKey, err := authz.IngestCredentialFrom([]string{"Bearer " + ingestKey}); err != nil || got != ingestKey || isAPIKey {
		t.Fatalf("ingest key: %q %v %v", got, isAPIKey, err)
	}
	if got, isAPIKey, err := authz.IngestCredentialFrom([]string{"Bearer " + apiKey}); err != nil || got != apiKey || !isAPIKey {
		t.Fatalf("API key: %q %v %v", got, isAPIKey, err)
	}
	for _, values := range [][]string{
		nil,
		{},
		{"Bearer " + apiKey, "Bearer " + apiKey},
		{"Bearer " + apiKey, "Bearer " + ingestKey},
		{"Bearer " + ingestKey, "Bearer " + ingestKey},
		{"Bearer ezgh_at_0123456789abcdefghijABCDEFGHIJklmnopqrstuvw"},
		{"Bearer ezgh_sat_0123456789abcdefghijABCDEFGHIJklmnopqrstuvw"},
		{"Bearer ezgh_ct_0123456789abcdefghijABCDEFGHIJklmnopqrstuvw"},
		{"Bearer ezgh_fs_0123456789abcdefghijABCDEFGHIJklmnopqrstuvw"},
		{apiKey},
		{ingestKey},
		{"Basic " + apiKey},
		{"Bearer pk_3k9d0a1m2x7q4b8c.AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"},
	} {
		if got, isAPIKey, err := authz.IngestCredentialFrom(values); err == nil || err.Status != 401 || err.Code != "unauthenticated" || got != "" || isAPIKey {
			t.Errorf("%q: %q %v %v", values, got, isAPIKey, err)
		}
	}
}

// [being built] Product data-plane keys (Feature Flags' ezgh_fs_ and ezgh_fc_ SDK keys) are taken
// only on their product's data host: no kit credential parser accepts one, and a product route
// refuses one with 401 without falling back to the cookie (contracts/identity-and-authz.md,
// "Product data-plane keys").
func TestProductDataPlaneKeysAreRefused(t *testing.T) {
	for _, key := range []string{
		"ezgh_fs_0123456789abcdefghijABCDEFGHIJklmnopqrstuvw",
		"ezgh_fc_0123456789abcdefghijABCDEFGHIJklmnopqrstuvw",
	} {
		header := "Bearer " + key
		if kind := authz.Classify(header); kind != "" {
			t.Errorf("%s classified as %q", key, kind)
		}
		if kind := authz.CustomerCredentialKind(header); kind != "" {
			t.Errorf("%s is a customer credential kind %q", key, kind)
		}
		if _, err := authz.IngestKeyFrom([]string{header}); err == nil || err.Status != 401 {
			t.Errorf("%s: ingest host accepted it: %v", key, err)
		}
		for _, opts := range [][]authz.CredentialOption{nil, {authz.BearerOnly()}} {
			r := httptest.NewRequest("GET", "/", nil)
			r.Header.Set("Authorization", header)
			r.Header.Set("Cookie", "better-auth.session_token=abc")
			cred, err := authz.CredentialFrom(r, opts...)
			if err == nil || err.Status != 401 || err.Code != "unauthenticated" || cred.Value != "" {
				t.Errorf("%s: %+v %v", key, cred, err)
			} else if !strings.Contains(err.Message, "data-plane key") {
				t.Errorf("%s: message %q doesn't say what the key is for", key, err.Message)
			}
		}
	}
}

// An OAuth access token goes to org-api exactly as it came, alone, like an API key.
func TestCheckForwardsAnOAuthAccessTokenAlone(t *testing.T) {
	const token = "Bearer ezgh_at_0123456789abcdefghijABCDEFGHIJklmnopqrstuvw"
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"organization":{"id":"o","slug":"org_k3f9a0x2m7qp"},"principal":{"type":"user","id":"u"},"apiKeyId":null,`+
			`"userIdentity":{"type":"User","principalId":"u","userName":"ada@acme.test","isRoot":false,"accessKeyId":null,`+
			`"sessionContext":{"credential":"oauth","sessionIssuedAt":"2026-09-29T08:00:00.000Z","ssoProviderId":null,"mfaAuthenticated":false,"clientId":"ezgh-cli"},"invokedBy":null},`+
			`"resource":"ezgh::org_k3f9a0x2m7qp","allowed":["a.b.c"]}`)
	}))
	defer server.Close()
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", token)
	r.Header.Set("Cookie", "better-auth.session_token=abc")
	cred, err := authz.CredentialFrom(r)
	if err != nil || !cred.IsOAuth() || cred.IsAPIKey() {
		t.Fatalf("%+v %v", cred, err)
	}
	p, err := (&authz.Client{BaseURL: server.URL}).Check(context.Background(), cred, "o", []string{"a.b.c"}, "")
	if err != nil || !p.Allows("a.b.c") || p.APIKeyID != nil || !strings.Contains(string(p.UserIdentity), `"clientId":"ezgh-cli"`) {
		t.Fatalf("%+v %v", p, err)
	}
	if got.Get("Authorization") != token || got.Get("Cookie") != "" {
		t.Errorf("sent %v", got)
	}
}

func TestCheckForwardsAnSSOResourceTokenAlone(t *testing.T) {
	const token = "Bearer ezgh_sat_0123456789abcdefghijABCDEFGHIJklmnopqrstuvw"
	var got []http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Clone())
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"organization":{"id":"o","slug":"org_k3f9a0x2m7qp"},"principal":{"type":"ssoUser","id":"u"},"apiKeyId":null,"userIdentity":{"type":"SsoUser","principalId":"u","userName":"ada@acme.test","isRoot":false,"accessKeyId":null,"sessionContext":{"credential":"ssoOauth","sessionIssuedAt":null,"ssoProviderId":null,"mfaAuthenticated":true,"directoryId":"01990000-0000-7000-8000-000000000001","clientId":"mcp-client","resource":"https://mcp.ezghcloud.com/v1/gateway"},"invokedBy":null},"allowed":[]}`)
	}))
	defer server.Close()
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", token)
	r.Header.Set("Cookie", "better-auth.session_token=abc")
	r.Header.Set("X-Forwarded-For", "198.51.100.7, 203.0.113.9")
	cred, err := authz.CredentialFrom(r)
	if err != nil || cred.IsAPIKey() || cred.IsOAuth() || !cred.IsBearer() {
		t.Fatalf("%+v %v", cred, err)
	}
	client := &authz.Client{BaseURL: server.URL, PublicBaseURL: "https://mcp.ezghcloud.com"}
	for i := 0; i < 2; i++ {
		permissions, err := client.Check(context.Background(), cred, "o", []string{"a.b.c"}, "")
		if err != nil || permissions.Allows("a.b.c") || permissions.Principal.Type != "ssoUser" || !strings.Contains(string(permissions.UserIdentity), `"credential":"ssoOauth"`) || !strings.Contains(string(permissions.UserIdentity), `"directoryId":`) || !strings.Contains(string(permissions.UserIdentity), `"resource":`) {
			t.Fatalf("want a denied SSO identity, got %+v %v", permissions, err)
		}
	}
	if len(got) != 2 {
		t.Fatalf("got %d checks, want 2", len(got))
	}
	for _, headers := range got {
		if headers.Get("Authorization") != token || headers.Get("Cookie") != "" || headers.Get("X-Forwarded-For") != "198.51.100.7, 203.0.113.9" {
			t.Errorf("sent %v", headers)
		}
	}
}

func TestCredentialFrom(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Cookie", "__Secure-better-auth.session_token=abc; theme=dark; better-auth.session_data=d")
	c, err := authz.CredentialFrom(r)
	if err != nil || c.Header != "Cookie" || c.Value != "__Secure-better-auth.session_token=abc; better-auth.session_data=d" || c.IsAPIKey() {
		t.Errorf("%+v %v", c, err)
	}
	r.Header.Set("Authorization", "Bearer ezgh_0123456789012345678901234567890123456789")
	if c, err := authz.CredentialFrom(r); err != nil || !c.IsAPIKey() {
		t.Errorf("the API key didn't win: %+v %v", c, err)
	}
	r.Header.Set("Authorization", "Basic abc")
	if _, err := authz.CredentialFrom(r); err == nil || err.Status != 401 {
		t.Error("accepted Basic")
	}
}

// The request's X-Forwarded-For goes to org-api with the credential, unchanged.
func TestCheckForwardsTheClientsForwardedFor(t *testing.T) {
	var got []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("X-Forwarded-For"))
		w.WriteHeader(404)
	}))
	defer server.Close()
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Cookie", "better-auth.session_token=abc")
	r.Header.Add("X-Forwarded-For", "198.51.100.7")
	r.Header.Add("X-Forwarded-For", "203.0.113.9")
	cred, _ := authz.CredentialFrom(r)
	if cred.ForwardedFor != "198.51.100.7, 203.0.113.9" {
		t.Errorf("ForwardedFor %q", cred.ForwardedFor)
	}
	c := &authz.Client{BaseURL: server.URL}
	_, _ = c.Check(context.Background(), cred, "o", []string{"a.b.c"}, "")
	_, _ = c.Check(context.Background(), authz.Credential{Header: "Cookie", Value: "better-auth.session_token=abc"}, "o", []string{"a.b.c"}, "")
	if len(got) != 2 || got[0] != "198.51.100.7, 203.0.113.9" || got[1] != "" {
		t.Errorf("org-api saw X-Forwarded-For %q", got)
	}
}

// org-api's 429 (the caller is over auth-api's rate limit) is passed on with its Retry-After.
func TestCheckPassesOnTheRateLimit(t *testing.T) {
	for header, want := range map[string]int{"7": 7, "": 1, "soon": 1, "0": 1} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if header != "" {
				w.Header().Set("Retry-After", header)
			}
			w.WriteHeader(429)
		}))
		_, err := (&authz.Client{BaseURL: server.URL}).Check(context.Background(), authz.Credential{Header: "Cookie", Value: "x"}, "o", []string{"a.b.c"}, "")
		if err == nil || err.Status != 429 || err.Code != "too_many_requests" || err.RetryAfter != want {
			t.Errorf("Retry-After %q → %+v, want %d", header, err, want)
		}
		server.Close()
	}
}

// The permission answer's statuses map to the caller's errors, and nothing fails open.
func TestCheckMapsOrgAPIAnswers(t *testing.T) {
	for status, want := range map[int]string{401: "unauthenticated", 404: "not_found", 400: "internal_error", 429: "too_many_requests", 500: "unavailable", 502: "unavailable", 503: "unavailable"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
		c := &authz.Client{BaseURL: server.URL}
		_, err := c.Check(context.Background(), authz.Credential{Header: "Cookie", Value: "better-auth.session_token=x"}, "o", []string{"a.b.c"}, "")
		if err == nil || err.Code != want {
			t.Errorf("%d → %v, want %s", status, err, want)
		}
		server.Close()
	}
	down := &authz.Client{BaseURL: "http://127.0.0.1:1"}
	if _, err := down.Check(context.Background(), authz.Credential{Header: "Cookie", Value: "x"}, "o", nil, ""); err == nil || err.Code != "unavailable" {
		t.Errorf("unreachable → %v", err)
	}
}

func TestLookup(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Header.Get("X-Internal-Token") != "tok" || !strings.Contains(string(body), `"projectIds":["0199a3c2-6c2f-7e51-8a4b-3d9f7c2e5a81","0199a3c2-6c2f-7e51-8a4b-3d9f7c2e5a82"]`) {
			w.WriteHeader(401)
			return
		}
		_, _ = w.Write([]byte(`{"organizations":[{"id":"0199a3c2-5b1e-7d40-9f3a-2c8e6b1d4f70","slug":"org_k3f9a0x2m7qp","state":"deletion_pending"}],
			"projects":[{"id":"0199a3c2-6c2f-7e51-8a4b-3d9f7c2e5a81","organizationId":"0199a3c2-5b1e-7d40-9f3a-2c8e6b1d4f70","slug":"prj_g7h8i9j0k1l2","deletedAt":null},
			{"id":"0199a3c2-6c2f-7e51-8a4b-3d9f7c2e5a82","organizationId":"0199a3c2-5b1e-7d40-9f3a-2c8e6b1d4f70","slug":"prj_aaaaaaaaaaaa","deletedAt":"2026-09-28T10:00:00.000Z"}]}`))
	}))
	defer server.Close()
	d, err := (&authz.Client{BaseURL: server.URL, InternalToken: "tok"}).Lookup(context.Background(), []string{"0199a3c2-5b1e-7d40-9f3a-2c8e6b1d4f70"}, []string{"0199a3c2-6c2f-7e51-8a4b-3d9f7c2e5a81", "0199a3c2-6c2f-7e51-8a4b-3d9f7c2e5a82"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Organizations["0199a3c2-5b1e-7d40-9f3a-2c8e6b1d4f70"].State != authz.StateDeletionPending {
		t.Error("state")
	}
	if _, ok := d.Project("0199a3c2-5b1e-7d40-9f3a-2c8e6b1d4f70", "0199a3c2-6c2f-7e51-8a4b-3d9f7c2e5a81"); !ok {
		t.Error("p1 is live")
	}
	if _, ok := d.Project("0199a3c2-5b1e-7d40-9f3a-2c8e6b1d4f70", "0199a3c2-6c2f-7e51-8a4b-3d9f7c2e5a82"); ok {
		t.Error("p2 is a tombstone")
	}
	if _, ok := d.Project("0199a3c2-5b1e-7d40-9f3a-2c8e6b1d4f71", "0199a3c2-6c2f-7e51-8a4b-3d9f7c2e5a81"); ok {
		t.Error("p1 isn't in o2")
	}
}

func TestLookupRejectsIncompleteResponses(t *testing.T) {
	for _, body := range []string{`{}`, `null`, `{"organizations":[],"projects":null}`, `{"organizations":[{}],"projects":[]}`, `{"organizations":[{"id":"0199a3c2-5b1e-7d40-9f3a-2c8e6b1d4f70","slug":"org_k3f9a0x2m7qp","state":"unknown"}],"projects":[]}`} {
		t.Run(body, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, body) }))
			defer s.Close()
			if _, err := (&authz.Client{BaseURL: s.URL}).Lookup(context.Background(), []string{"0199a3c2-5b1e-7d40-9f3a-2c8e6b1d4f70"}, nil); err == nil {
				t.Fatal("accepted unsafe directory response")
			}
		})
	}
}
func TestLookupRejectsUnexpectedIDs(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"organizations":[{"id":"0199a3c2-5b1e-7d40-9f3a-2c8e6b1d4f71","slug":"org_aaaaaaaaaaaa","state":"active"}],"projects":[]}`)
	}))
	defer s.Close()
	if _, err := (&authz.Client{BaseURL: s.URL}).Lookup(context.Background(), []string{"0199a3c2-5b1e-7d40-9f3a-2c8e6b1d4f70"}, nil); err == nil {
		t.Fatal("accepted unrelated directory data")
	}
}

const ssoToken = "Bearer ezgh_sat_0123456789abcdefghijABCDEFGHIJklmnopqrstuvw"

// fakePermissions is an org-api that records each permissions request's query and answers 200.
func fakePermissions(t *testing.T) (*httptest.Server, *[]*http.Request) {
	t.Helper()
	var got []*http.Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Clone(context.Background()))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"organization":{"id":"o","slug":"org_k3f9a0x2m7qp"},"principal":{"type":"ssoUser","id":"u"},"apiKeyId":null,"userIdentity":{"type":"SsoUser"},"allowed":["a.b.c"]}`)
	}))
	t.Cleanup(server.Close)
	return server, &got
}

// contracts/vectors/resource-binding.json's requestUrls, through CredentialFrom and Check: an
// ezgh_sat_ token sends org-api requestUrl, the configured base URL's origin plus the raw path of
// the request-target, whatever Host and X-Forwarded-* say. A path with no canonical form is 401
// without calling org-api; a base URL that isn't an https origin never reaches org-api either
// (config refuses to start with one).
func TestCheckSendsRequestURLForAnSSOResourceToken(t *testing.T) {
	var file struct {
		RequestURLs []struct {
			Name          string            `json:"name"`
			PublicBaseURL string            `json:"publicBaseUrl"`
			RequestTarget string            `json:"requestTarget"`
			Headers       map[string]string `json:"headers"`
			RequestURL    *string           `json:"requestUrl"`
		} `json:"requestUrls"`
	}
	vectors.Load(t, "resource-binding.json", &file)
	for _, c := range file.RequestURLs {
		server, got := fakePermissions(t)
		r := httptest.NewRequest("GET", "/", nil)
		r.RequestURI = c.RequestTarget
		for name, value := range c.Headers {
			r.Header.Set(name, value)
		}
		if host, ok := c.Headers["Host"]; ok {
			r.Host = host
		}
		r.Header.Set("Authorization", ssoToken)
		cred, err := authz.CredentialFrom(r)
		if err != nil || !cred.IsSSO() || cred.RequestTarget != c.RequestTarget {
			t.Fatalf("%s: %+v %v", c.Name, cred, err)
		}
		_, checkErr := (&authz.Client{BaseURL: server.URL, PublicBaseURL: c.PublicBaseURL}).Check(context.Background(), cred, "o", []string{"a.b.c"}, "ezgh::org_k3f9a0x2m7qp")
		_, baseOK := resourceurl.Origin(c.PublicBaseURL)
		switch {
		case c.RequestURL != nil:
			if checkErr != nil || len(*got) != 1 {
				t.Errorf("%s: %v, %d calls", c.Name, checkErr, len(*got))
				continue
			}
			sent := (*got)[0]
			want := url.Values{"actions": {"a.b.c"}, "resource": {"ezgh::org_k3f9a0x2m7qp"}, "requestUrl": {*c.RequestURL}}
			if sent.URL.RawQuery != want.Encode() {
				t.Errorf("%s: sent %s, want %s", c.Name, sent.URL.RawQuery, want.Encode())
			}
			if sent.Header.Get("Authorization") != ssoToken {
				t.Errorf("%s: sent %v", c.Name, sent.Header)
			}
		case !baseOK:
			if checkErr == nil || checkErr.Status != 500 || len(*got) != 0 {
				t.Errorf("%s: a bad base URL gave %v after %d calls", c.Name, checkErr, len(*got))
			}
		default:
			if checkErr == nil || checkErr.Status != 401 || checkErr.Code != "unauthenticated" || len(*got) != 0 {
				t.Errorf("%s: a non-canonical path gave %v after %d calls", c.Name, checkErr, len(*got))
			}
		}
	}
}

// requestUrl is for ezgh_sat_ only: no other credential sends it, even on a client that has a
// base URL, and no other credential keeps the request-target.
func TestCheckSendsRequestURLForNothingElse(t *testing.T) {
	for _, header := range []string{
		"Bearer ezgh_0123456789012345678901234567890123456789",
		"Bearer ezgh_at_0123456789abcdefghijABCDEFGHIJklmnopqrstuvw",
		"",
	} {
		server, got := fakePermissions(t)
		r := httptest.NewRequest("GET", "/v1/organizations/o/widgets", nil)
		if header != "" {
			r.Header.Set("Authorization", header)
		} else {
			r.Header.Set("Cookie", "better-auth.session_token=abc")
		}
		cred, err := authz.CredentialFrom(r)
		if err != nil || cred.IsSSO() || cred.RequestTarget != "" {
			t.Fatalf("%q: %+v %v", header, cred, err)
		}
		if _, err := (&authz.Client{BaseURL: server.URL, PublicBaseURL: "https://widgets.ezghcloud.com"}).Check(context.Background(), cred, "o", []string{"a.b.c"}, ""); err != nil || len(*got) != 1 {
			t.Fatalf("%q: %v", header, err)
		}
		if q := (*got)[0].URL.Query(); q.Has("requestUrl") {
			t.Errorf("%q: sent requestUrl %q", header, q.Get("requestUrl"))
		}
	}
}

// Over a real server: the raw path as received (not decoded), the query string left out, and the
// requestUrl percent-encoded as a query value.
func TestCheckSendsTheRawRequestPath(t *testing.T) {
	orgAPI, got := fakePermissions(t)
	client := &authz.Client{BaseURL: orgAPI.URL, PublicBaseURL: "https://MCP.ezghcloud.com:443/"}
	product := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cred, err := authz.CredentialFrom(r)
		if err == nil {
			_, err = client.Check(r.Context(), cred, "o", []string{"a.b.c"}, "")
		}
		if err != nil {
			w.WriteHeader(err.Status)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer product.Close()
	for path, want := range map[string]string{
		"/v1/organizations/o/gateways/g/mcp?session=1&x=%2F": "https://mcp.ezghcloud.com/v1/organizations/o/gateways/g/mcp",
		"/v1/%41b/":    "https://mcp.ezghcloud.com/v1/%41b/",
		"/v1/a%2Fb":    "",
		"/v1/%2e%2e/x": "",
	} {
		*got = nil
		req, _ := http.NewRequest("GET", product.URL+path, nil)
		req.Header.Set("Authorization", ssoToken)
		req.Header.Set("X-Forwarded-Host", "evil.example")
		req.Header.Set("X-Forwarded-Proto", "http")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if want == "" {
			if resp.StatusCode != 401 || len(*got) != 0 {
				t.Errorf("%s: %d after %d calls", path, resp.StatusCode, len(*got))
			}
			continue
		}
		if resp.StatusCode != 204 || len(*got) != 1 {
			t.Fatalf("%s: %d after %d calls", path, resp.StatusCode, len(*got))
		}
		raw := (*got)[0].URL.RawQuery
		if !strings.Contains(raw, "requestUrl="+url.QueryEscape(want)) {
			t.Errorf("%s: sent %s, want requestUrl=%s", path, raw, url.QueryEscape(want))
		}
	}
}

// BearerOnly never reads cookies: cookies alone are 401, and a bearer credential wins as usual.
func TestCredentialFromBearerOnly(t *testing.T) {
	r := httptest.NewRequest("POST", "/central", nil)
	r.AddCookie(&http.Cookie{Name: "__Secure-better-auth.session_token", Value: "abc.def"})
	if _, err := authz.CredentialFrom(r, authz.BearerOnly()); err == nil || err.Status != 401 {
		t.Fatalf("cookie only: %v", err)
	}
	if cred, err := authz.CredentialFrom(r); err != nil || cred.Header != "Cookie" {
		t.Fatalf("without the option the cookie is the credential: %+v %v", cred, err)
	}
	r.Header.Set("Authorization", "Bearer ezgh_sat_0123456789abcdefghijABCDEFGHIJklmnopqrstuvw")
	cred, err := authz.CredentialFrom(r, authz.BearerOnly())
	if err != nil || !cred.IsSSO() || cred.RequestTarget != "/central" {
		t.Fatalf("%+v %v", cred, err)
	}
	r.Header.Set("Authorization", "Bearer ezgh_rt_0123456789abcdefghijABCDEFGHIJklmnopqrstuvw")
	if _, err := authz.CredentialFrom(r, authz.BearerOnly()); err == nil || err.Status != 401 {
		t.Fatalf("a refresh token: %v", err)
	}
}

// contracts/vectors/product-cookie.json: ProductCookie takes only the named cookie's EZGH SSO
// resource token, forwarded as a bearer credential and marked as a cookie's.
func TestProductCookieVectors(t *testing.T) {
	var file struct {
		CookieName string `json:"cookieName"`
		Cases      []struct {
			Name          string  `json:"name"`
			Cookie        *string `json:"cookie"`
			Authorization *string `json:"authorization"`
			Expected      *string `json:"expected"`
		} `json:"cases"`
	}
	vectors.Load(t, "product-cookie.json", &file)
	if len(file.Cases) < 10 {
		t.Fatalf("only %d cases", len(file.Cases))
	}
	for _, c := range file.Cases {
		r := httptest.NewRequest("GET", "/v1/me?x=1", nil)
		if c.Cookie != nil {
			r.Header.Set("Cookie", *c.Cookie)
		}
		if c.Authorization != nil {
			r.Header.Set("Authorization", *c.Authorization)
		}
		cred, err := authz.CredentialFrom(r, authz.ProductCookie(file.CookieName))
		switch {
		case c.Expected == nil && err == nil:
			t.Errorf("%s: accepted %+v", c.Name, cred)
		case c.Expected == nil && err.Status != 401:
			t.Errorf("%s: %v", c.Name, err)
		case c.Expected != nil && (err != nil || cred.Value != *c.Expected || cred.Header != "Authorization" || !cred.FromProductCookie || !cred.IsSSO() || cred.IsBearer() || cred.RequestTarget != "/v1/me?x=1"):
			t.Errorf("%s: %+v %v", c.Name, cred, err)
		}
	}
}

// A fixed requestUrl (a product cookie's audience label) is sent as is, in place of the URL the
// request is at, and must be canonical.
func TestFixedRequestURL(t *testing.T) {
	var got url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		w.WriteHeader(401)
	}))
	defer server.Close()
	client := &authz.Client{BaseURL: server.URL, PublicBaseURL: "https://mcpcentral.ezghcloud.com"}
	label := "https://mcpcentral.ezghcloud.com/v1/organizations/0199a3c2-5b1e-7d40-9f3a-2c8e6b1d4f70/portal"
	cred := authz.Credential{Header: "Authorization", Value: "Bearer ezgh_sat_0123456789abcdefghijABCDEFGHIJklmnopqrstuvw", RequestTarget: "/v1/me", FromProductCookie: true}.WithRequestURL(label)
	if _, err := client.Check(context.Background(), cred.WithPublicBase("https://acme.mcp.ezgh.app"), "o", []string{"a.b.c"}, ""); err == nil || err.Status != 401 || got.Get("requestUrl") != label {
		t.Fatalf("%v %v", err, got)
	}
	if _, err := client.Check(context.Background(), cred.WithRequestURL("https://MCPCENTRAL.ezghcloud.com/v1/x"), "o", nil, ""); err == nil || err.Status != 500 {
		t.Errorf("a non-canonical label: %v", err)
	}
}

// LookupSlugs sends the prefixed IDs in organizationSlugs and projectSlugs, and accepts only
// entries the request named.
func TestLookupSlugs(t *testing.T) {
	answer := `{"organizations":[{"id":"0199a3c2-5b1e-7d40-9f3a-2c8e6b1d4f70","slug":"org_k3f9a0x2m7qp","state":"active"}],
		"projects":[{"id":"0199a3c2-6c2f-7e51-8a4b-3d9f7c2e5a81","organizationId":"0199a3c2-5b1e-7d40-9f3a-2c8e6b1d4f70","slug":"prj_g7h8i9j0k1l2","deletedAt":null}]}`
	var sent string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sent = string(body)
		_, _ = io.WriteString(w, answer)
	}))
	defer server.Close()
	c := &authz.Client{BaseURL: server.URL, InternalToken: "tok"}
	d, err := c.LookupSlugs(context.Background(), []string{"org_k3f9a0x2m7qp"}, []string{"prj_g7h8i9j0k1l2"})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"organizationIds":[],"organizationSlugs":["org_k3f9a0x2m7qp"],"projectIds":[],"projectSlugs":["prj_g7h8i9j0k1l2"]}`; sent != want {
		t.Errorf("sent %s, want %s", sent, want)
	}
	if d.Organizations["0199a3c2-5b1e-7d40-9f3a-2c8e6b1d4f70"].Slug != "org_k3f9a0x2m7qp" || d.Projects["0199a3c2-6c2f-7e51-8a4b-3d9f7c2e5a81"].Slug != "prj_g7h8i9j0k1l2" {
		t.Errorf("directory %+v", d)
	}
	// An entry the request didn't name is a bad answer.
	if _, err := c.LookupSlugs(context.Background(), []string{"org_aaaaaaaaaaaa"}, nil); err == nil {
		t.Error("accepted an organization nobody asked for")
	}
	// Lookup by IDs never sends the slug fields (org-api before them refuses unknown fields).
	_, _ = c.Lookup(context.Background(), []string{"0199a3c2-5b1e-7d40-9f3a-2c8e6b1d4f70"}, nil)
	if strings.Contains(sent, "Slugs") {
		t.Errorf("Lookup sent %s", sent)
	}
}

// A KubernetesCluster key's cluster is its sourceIdentity's principalId (the cluster's UUID); every
// other source, none, and malformed JSON give "".
func TestPermissionsKubernetesCluster(t *testing.T) {
	const cluster = "0199f3a1-6d2e-7c40-8b1a-3e5f7a9c0d12"
	identity := func(source string) string {
		return `{"type":"Bot","principalId":"b","userName":"n","isRoot":false,"accessKeyId":"k","sessionContext":{"credential":"apiKey","sessionIssuedAt":null,"ssoProviderId":null,"mfaAuthenticated":false` + source + `},"invokedBy":null}`
	}
	for _, c := range []struct {
		name, identity, want string
	}{
		{"cluster", identity(`,"sourceIdentity":{"type":"KubernetesCluster","principalId":"` + cluster + `","userName":"prod"}`), cluster},
		{"pod", identity(`,"sourceIdentity":{"type":"KubernetesPod","principalId":"` + cluster + `","userName":"ns/sa"}`), ""},
		{"instance", identity(`,"sourceIdentity":{"type":"ComputeInstance","principalId":"` + cluster + `","userName":"web-1"}`), ""},
		{"no source", identity(``), ""},
		{"no session", `{"type":"User","principalId":"u","sessionContext":null}`, ""},
		{"malformed", `{"type":`, ""},
		{"empty", ``, ""},
	} {
		p := &authz.Permissions{UserIdentity: []byte(c.identity)}
		if got := p.KubernetesCluster(); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
