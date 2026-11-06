package logging

import (
	"strings"
	"unicode"
)

// Redacted is what a sensitive value is written as (contracts/deployment.md, "Logging and
// shutdown").
const Redacted = "[REDACTED]"

// The lists are contracts/vectors/logging.json's "sensitive"; TestVectorLists keeps them equal.
var (
	sensitiveOne = set("token", "secret", "password", "passwd", "passphrase", "cookie", "cookies",
		"authorization", "credential", "credentials", "signature", "apikey", "privatekey", "cvc", "cvv", "otp")
	sensitiveTwo = set("api key", "private key", "secret key", "signing key", "encryption key", "access key",
		"application key", "card number", "password hash", "secret hash", "token hash")
	containers = set("value", "values", "raw", "pem", "der", "header", "headers", "bytes", "b64", "base64",
		"hex", "plain", "plaintext", "string", "str", "text", "data")
)

func set(words ...string) map[string]bool {
	m := make(map[string]bool, len(words))
	for _, w := range words {
		m[w] = true
	}
	return m
}

// Sensitive reports whether a value logged under key is written as Redacted: the key's words,
// less trailing container words ("value", "pem"…), end in a sensitive word or pair. Matching the
// end keeps identifiers and counts: apiKeyId, prompt_tokens and token_endpoint are kept.
func Sensitive(key string) bool {
	w := words(key)
	for len(w) > 0 && containers[w[len(w)-1]] {
		w = w[:len(w)-1]
	}
	n := len(w)
	if n == 0 {
		return false
	}
	return sensitiveOne[w[n-1]] || (n >= 2 && sensitiveTwo[w[n-2]+" "+w[n-1]])
}

// words splits a key at _ - . and spaces and at camelCase boundaries, lowercased: "x-api-key",
// "apiKey" and "APIKey" all end in "api key".
func words(key string) []string {
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			out = append(out, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	rs := []rune(key)
	for i, r := range rs {
		if r == '_' || r == '-' || r == '.' || r == ' ' {
			flush()
			continue
		}
		if i > 0 && unicode.IsUpper(r) {
			prev := rs[i-1]
			nextLower := i+1 < len(rs) && unicode.IsLower(rs[i+1])
			if unicode.IsLower(prev) || ('0' <= prev && prev <= '9') || (unicode.IsUpper(prev) && nextLower) {
				flush()
			}
		}
		cur = append(cur, r)
	}
	flush()
	return out
}

// Redact masks sensitive keys at any depth of a JSON-shaped value (the maps and slices
// encoding/json decodes into); anything else is returned as it is.
func Redact(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			if Sensitive(k) {
				out[k] = Redacted
			} else {
				out[k] = Redact(x)
			}
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = Redact(x)
		}
		return out
	}
	return v
}
