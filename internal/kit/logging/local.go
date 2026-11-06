package logging

import (
	"context"
	"log/slog"
)

// localHandler is slog's text handler with the request's context fields added.
type localHandler struct{ slog.Handler }

func (h localHandler) Handle(ctx context.Context, r slog.Record) error {
	c := contextOf(ctx)
	for i, v := range [...]string{c.RequestID, c.OrganizationID, c.TraceID, c.SpanID} {
		if v != "" {
			r.AddAttrs(slog.String(contextKeys[i], v))
		}
	}
	return h.Handler.Handle(ctx, r)
}

func (h localHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return localHandler{h.Handler.WithAttrs(as)}
}
func (h localHandler) WithGroup(n string) slog.Handler { return localHandler{h.Handler.WithGroup(n)} }

// redactAttr is the text handler's redaction: sensitive keys and groups, and values inside
// anything it would print with fmt.
func redactAttr(groups []string, a slog.Attr) slog.Attr {
	for _, g := range groups {
		if Sensitive(g) {
			return slog.String(a.Key, Redacted)
		}
	}
	if Sensitive(a.Key) {
		return slog.String(a.Key, Redacted)
	}
	if a.Value.Kind() == slog.KindAny {
		return slog.Any(a.Key, Redact(anyValue(a.Value.Any())))
	}
	return a
}
