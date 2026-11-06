package ezghclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
)

type idempotencyKey struct{}

// WithIdempotencyKey makes the POST sent with the returned context carry a key derived from key
// and the request's path instead of a random one, so a controller that crashed after a create and
// before recording it gets the same resource back. Use it around one create call only, never a
// whole reconcile: the service replays any POST to that path with that key for 24 hours. The
// copied clients stay unedited: the transport swaps the header.
func WithIdempotencyKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, idempotencyKey{}, key)
}

type keyTransport struct{ next http.RoundTripper }

func (t keyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if key, ok := r.Context().Value(idempotencyKey{}).(string); ok && key != "" && r.Method == http.MethodPost && r.Header.Get("Idempotency-Key") != "" {
		sum := sha256.Sum256([]byte(r.URL.Path))
		r = r.Clone(r.Context())
		r.Header.Set("Idempotency-Key", key+"-"+hex.EncodeToString(sum[:6]))
	}
	return t.next.RoundTrip(r)
}
