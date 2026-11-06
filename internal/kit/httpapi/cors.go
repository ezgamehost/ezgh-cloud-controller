package httpapi

import (
	"net/http"
	"slices"
	"strings"
)

// Origins is the CORS allowlist, CORS_ORIGINS. Any is "*", allowed only with APP_ENV=local, and
// means "reflect the request's origin": credentialed requests can't use a literal *.
type Origins struct {
	Any  bool
	List []string
}

// Allowed returns the origin to answer with, or "" when origin isn't allowed. A request without
// an Origin is never allowed, so a cookie write must carry one.
func (o Origins) Allowed(origin string) string {
	if origin == "" {
		return ""
	}
	if o.Any || slices.Contains(o.List, origin) {
		return origin
	}
	return ""
}

// The CORS contract, the same for every product.
const (
	corsMethods        = "GET, POST, PUT, PATCH, DELETE, OPTIONS"
	corsRequestHeaders = "Content-Type, Authorization, Idempotency-Key, If-Match, X-Request-Id"
	corsExposedHeaders = "X-Request-Id, Retry-After, Location, Content-Disposition, Idempotent-Replayed, ETag"
)

// CORS answers preflights and adds the credentialed CORS headers for allowed origins. It never
// refuses anything itself: the browser enforces CORS, and OriginCheck refuses cookie writes.
func CORS(origins Origins, next http.Handler) http.Handler { return CORSExcept(origins, nil, next) }

// CORSExcept is CORS, except that requests skip(r) is true for get no CORS headers and no
// preflight answer (the router answers them as any other request): server-to-server endpoints
// such as invocation-token endpoints.
func CORSExcept(origins Origins, skip func(*http.Request) bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if skip != nil && skip(r) {
			next.ServeHTTP(w, r)
			return
		}
		h := w.Header()
		h.Add("Vary", "Origin")
		allowed := origins.Allowed(r.Header.Get("Origin"))
		if allowed != "" {
			h.Set("Access-Control-Allow-Origin", allowed)
			h.Set("Access-Control-Allow-Credentials", "true")
			h.Set("Access-Control-Expose-Headers", corsExposedHeaders)
		}
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			if allowed != "" {
				h.Set("Access-Control-Allow-Methods", corsMethods)
				h.Set("Access-Control-Allow-Headers", corsRequestHeaders)
				h.Set("Access-Control-Max-Age", "600")
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// The CORS that endpoints authorized only by what's in their body answer (contracts/api-conventions.md,
// "CORS and cross-site writes"): receipt-handle endpoints.
const (
	anyOriginMethods        = "POST, OPTIONS"
	anyOriginRequestHeaders = "Content-Type"
	anyOriginExposedHeaders = "X-Request-Id, Retry-After"
)

// CORSAnyOrigin answers every origin with Access-Control-Allow-Origin: * and never with
// Access-Control-Allow-Credentials, preflights included. It's for endpoints that read no ambient
// credential (no cookie, no Authorization), where the request's own body is the capability, so a
// page on any origin can call them and gains nothing a cross-site request could abuse.
func CORSAnyOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Expose-Headers", anyOriginExposedHeaders)
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			h.Set("Access-Control-Allow-Methods", anyOriginMethods)
			h.Set("Access-Control-Allow-Headers", anyOriginRequestHeaders)
			h.Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// IsWrite reports whether a method changes something (and so needs the Origin check when it's
// authenticated by the cookie).
func IsWrite(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	return true
}

// HasBearer reports whether the request carries Authorization: Bearer. Browsers never attach
// one on their own, so such requests can't be cross-site forgeries and skip the Origin check.
func HasBearer(r *http.Request) bool {
	return strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ")
}
