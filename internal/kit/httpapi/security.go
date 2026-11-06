package httpapi

import "net/http"

// HSTS is the Strict-Transport-Security every API response carries: a year, this host only (each
// API host is its own; the platform's other hosts set their own).
const HSTS = "max-age=31536000"

// WithSecurityHeaders gives every response X-Content-Type-Options: nosniff and
// Strict-Transport-Security (HSTS), whatever answers it: a route, the router's 404 or 405, or a
// CORS preflight (contracts/api-conventions.md, "Headers"). The Gateway terminates TLS and serves
// http:// only as a redirect, so browsers see HSTS on HTTPS only. A handler may still override them.
func WithSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Strict-Transport-Security", HSTS)
		next.ServeHTTP(w, r)
	})
}
