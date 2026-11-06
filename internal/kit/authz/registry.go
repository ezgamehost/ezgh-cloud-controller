package authz

import (
	"encoding/base64"
	"net/http"
	"strings"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/httpapi"
)

// RegistryAnonymousToken is the token a registry's token endpoint hands a client with no
// credential. On a registry host it means "no credential", and nothing else does.
const RegistryAnonymousToken = "anonymous"

// RegistryCredentialFrom is a registry host's credential check (contracts/identity-and-authz.md,
// "Registry credentials"). Exactly one Authorization header holding an API key is a credential:
// HTTP Basic with the key as the password (any username, as docker login sends it), or a bearer
// (what the token endpoint handed out). No header, or "Bearer anonymous", is no credential, for
// public repositories: it returns anonymous true. Cookies are never read. Anything else (OAuth
// access tokens, EZGH SSO resource tokens, ingest keys, service credentials, two headers, another
// scheme) is 401 without asking org-api. The credential is forwarded as "Bearer <key>" with the
// request's X-Forwarded-For, exactly as CredentialFrom forwards an API key.
func RegistryCredentialFrom(r *http.Request) (Credential, bool, *httpapi.Error) {
	values := r.Header.Values("Authorization")
	if len(values) == 0 {
		return Credential{}, true, nil
	}
	if len(values) > 1 {
		return Credential{}, false, registryUnauthenticated()
	}
	header := values[0]
	if header == "Bearer "+RegistryAnonymousToken {
		return Credential{}, true, nil
	}
	if encoded, ok := strings.CutPrefix(header, "Basic "); ok {
		password, ok := basicPassword(encoded)
		if !ok {
			return Credential{}, false, registryUnauthenticated()
		}
		header = "Bearer " + password
	}
	if CustomerCredentialKind(header) != KindAPIKey {
		return Credential{}, false, registryUnauthenticated()
	}
	return bearerCredential(r, header), false, nil
}

// RegistryToken is the API key itself, without "Bearer ": the token a registry's token endpoint
// returns. Nothing is minted, so nothing needs caching or revoking.
func RegistryToken(c Credential) string { return strings.TrimPrefix(c.Value, "Bearer ") }

// basicPassword is the password of HTTP Basic credentials, base64 of "<user>:<password>" (padded
// or not); ok is false for anything that isn't base64 or has no colon.
func basicPassword(encoded string) (string, bool) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		if raw, err = base64.RawStdEncoding.DecodeString(encoded); err != nil {
			return "", false
		}
	}
	_, password, ok := strings.Cut(string(raw), ":")
	return password, ok
}

func registryUnauthenticated() *httpapi.Error {
	return httpapi.Unauthenticated("Sign in with an EZGH API key: docker login -u <anything> --password-stdin")
}
