package authz_test

import (
	"net/http/httptest"
	"testing"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/authz"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/internal/vectors"
)

// contracts/vectors/registry-credentials.json: a registry host takes exactly one API key, over
// Basic (the password) or Bearer; no header or "Bearer anonymous" is no credential; anything else
// is 401, and a cookie is never read.
func TestRegistryCredentialVectors(t *testing.T) {
	var file struct {
		Cases []struct {
			Name          string   `json:"name"`
			Authorization []string `json:"authorization"`
			Cookie        string   `json:"cookie"`
			Expect        string   `json:"expect"`
			Token         string   `json:"token"`
		} `json:"cases"`
	}
	vectors.Load(t, "registry-credentials.json", &file)
	if len(file.Cases) < 15 {
		t.Fatalf("only %d cases", len(file.Cases))
	}
	for _, c := range file.Cases {
		t.Run(c.Name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "https://df5jvhmlyoaf.registry.ezghcloud.com/v2/", nil)
			for _, v := range c.Authorization {
				r.Header.Add("Authorization", v)
			}
			if c.Cookie != "" {
				r.Header.Set("Cookie", c.Cookie)
			}
			r.Header.Set("X-Forwarded-For", "203.0.113.7")
			cred, anonymous, err := authz.RegistryCredentialFrom(r)
			got := "apiKey"
			switch {
			case err != nil:
				got = "unauthenticated"
				if err.Status != 401 {
					t.Fatalf("status %d", err.Status)
				}
				if anonymous || cred != (authz.Credential{}) {
					t.Fatalf("a refusal returned anonymous=%v, %+v", anonymous, cred)
				}
			case anonymous:
				got = "anonymous"
				if cred != (authz.Credential{}) {
					t.Fatalf("anonymous returned a credential %+v", cred)
				}
			}
			if got != c.Expect {
				t.Fatalf("got %s, want %s", got, c.Expect)
			}
			if got == "apiKey" {
				if authz.RegistryToken(cred) != c.Token || !cred.IsAPIKey() || !cred.IsBearer() || cred.ForwardedFor != "203.0.113.7" || cred.Header != "Authorization" || cred.Value != "Bearer "+c.Token {
					t.Fatalf("credential %+v", cred)
				}
			}
		})
	}
}

// The token a registry's token endpoint hands out is the key itself; with no credential, it's
// the anonymous token.
func TestRegistryTokenIsTheKey(t *testing.T) {
	r := httptest.NewRequest("GET", "/v2/token", nil)
	r.SetBasicAuth("someone", "ezgh_0123456789abcdefghijABCDEFGHIJ0123456789")
	cred, anonymous, err := authz.RegistryCredentialFrom(r)
	if err != nil || anonymous {
		t.Fatalf("anonymous=%v err=%v", anonymous, err)
	}
	if got := authz.RegistryToken(cred); got != "ezgh_0123456789abcdefghijABCDEFGHIJ0123456789" {
		t.Fatalf("token %q", got)
	}
	if cred.ForwardedFor != "" || cred.RequestTarget != "" {
		t.Fatalf("credential %+v", cred)
	}
	r = httptest.NewRequest("GET", "/v2/token", nil)
	r.Header.Set("Authorization", "Bearer "+authz.RegistryAnonymousToken)
	if _, anonymous, err := authz.RegistryCredentialFrom(r); err != nil || !anonymous {
		t.Fatalf("anonymous=%v err=%v", anonymous, err)
	}
}
