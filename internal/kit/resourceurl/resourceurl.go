// Package resourceurl is the one canonical form of an EZGH resource URL, and how a product builds
// the `requestUrl` it sends org-api with an EZGH SSO resource token (`Bearer ezgh_sat_…`)
// (contracts/api-conventions.md, "Canonical resource URLs"; contracts/identity-and-authz.md,
// "Resource binding"). sso-edge binds a token to a canonical URL; org-api compares that string
// with requestUrl byte for byte and never canonicalizes, so both sides must produce exactly this
// form. The reference implementation is scripts/lib/resource-binding.mjs, and
// contracts/vectors/resource-binding.json holds the cases.
//
// This is string work on purpose: generic URL parsers normalize (net/url, like a WHATWG parser,
// decodes and resolves), and two parsers needn't agree. Nothing here is decoded, re-encoded or
// resolved: a value is either already in the form or refused.
package resourceurl

import (
	"regexp"
	"strings"
)

// MaxLength is the longest canonical URL, in bytes (it's printable ASCII).
const MaxLength = 2048

var (
	scheme    = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*://`)
	hostLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
	// RFC 3986 path characters: unreserved, sub-delims, ":", "@", "/", and well-formed %XX.
	pathChars        = regexp.MustCompile(`^(?:[A-Za-z0-9\-._~!$&'()*+,;=:@/]|%[0-9A-Fa-f]{2})*$`)
	encodedSeparator = regexp.MustCompile(`%(?:2[Ff]|5[Cc]|2[Ee])`)
)

// Canonical is input's canonical form: https, the host lowercased, no port (":443" is dropped),
// no userinfo, query or fragment, and the path exactly as given. ok is false when input is
// refused: another scheme or port, a host that isn't LDH labels, a path with a dot segment,
// "//", a percent-encoded "/", "\" or ".", or a character RFC 3986 doesn't allow there, anything
// but printable ASCII, or more than MaxLength bytes.
func Canonical(input string) (string, bool) {
	if len(input) == 0 || len(input) > MaxLength {
		return "", false
	}
	// Printable ASCII only: whitespace, controls and raw non-ASCII (an IDN U-label host, a raw
	// UTF-8 path) are refused, never trimmed, converted or encoded.
	for i := 0; i < len(input); i++ {
		if input[i] < 0x21 || input[i] > 0x7e {
			return "", false
		}
	}
	prefix := scheme.FindString(input)
	if prefix == "" || !strings.EqualFold(prefix, "https://") {
		return "", false
	}
	rest := input[len(prefix):]
	// Any query or fragment, even an empty "?" or "#", is refused.
	if strings.ContainsAny(rest, "?#") {
		return "", false
	}
	authority, path := rest, ""
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		authority, path = rest[:i], rest[i:]
	}
	host, ok := hostOf(authority)
	if !ok || !pathIsCanonical(path) {
		return "", false
	}
	return "https://" + host + path, true
}

func hostOf(authority string) (string, bool) {
	if strings.Contains(authority, "@") { // userinfo
		return "", false
	}
	host := authority
	if colon := strings.LastIndexByte(authority, ':'); colon >= 0 {
		// The default port is dropped; any other port (or an empty or zero-padded one) is refused.
		if authority[colon+1:] != "443" {
			return "", false
		}
		host = authority[:colon]
	}
	host = strings.ToLower(host)
	if len(host) == 0 || len(host) > 253 {
		return "", false
	}
	// LDH labels: punycode (xn--) is just ASCII and stays as it is; no trailing dot.
	for _, label := range strings.Split(host, ".") {
		if !hostLabel.MatchString(label) {
			return "", false
		}
	}
	return host, true
}

func pathIsCanonical(path string) bool {
	if !strings.HasPrefix(path, "/") { // an empty path is refused, never made "/"
		return false
	}
	if !pathChars.MatchString(path) || strings.Contains(path, "//") || encodedSeparator.MatchString(path) {
		return false
	}
	for _, segment := range strings.Split(path[1:], "/") {
		if segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

// Origin is a configured public base URL's canonical origin, without a trailing slash
// ("https://widgets.ezghcloud.com"). ok is false unless publicBaseURL is an https origin: an
// https URL with nothing after the host but one optional "/" (no port but 443, no path, query or
// fragment). A product refuses to start with anything else.
func Origin(publicBaseURL string) (string, bool) {
	base := strings.TrimSuffix(publicBaseURL, "/")
	root, ok := Canonical(base + "/")
	if !ok || strings.Count(root, "/") != 3 {
		return "", false
	}
	return strings.TrimSuffix(root, "/"), true
}

// RequestURL is the requestUrl for a request: publicBaseURL's origin plus the raw path of the
// request-target as received (r.RequestURI: before the router decodes it), without its query
// string and, for an absolute-form request-target, without its authority. It's never built from
// Host, X-Forwarded-* or Forwarded, which the client chooses. ok is false when there's none: a
// publicBaseURL that isn't an https origin (a configuration error), or a path that isn't
// canonical, which can't be at any bound URL (the product answers 401 without calling org-api).
func RequestURL(publicBaseURL, requestTarget string) (string, bool) {
	origin, ok := Origin(publicBaseURL)
	if !ok {
		return "", false
	}
	// An absolute-form request-target contributes only its path, never its authority.
	target := requestTarget
	if prefix := scheme.FindString(target); prefix != "" {
		target = target[len(prefix):]
		if i := strings.IndexAny(target, "/?#"); i >= 0 {
			target = target[i:]
		} else {
			target = ""
		}
	}
	if i := strings.IndexAny(target, "?#"); i >= 0 {
		target = target[:i]
	}
	if !strings.HasPrefix(target, "/") {
		return "", false
	}
	return Canonical(origin + target)
}
