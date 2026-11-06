// Package logging is the service's log (contracts/deployment.md, "Logging and shutdown";
// docs/logging-design.md): JSON lines on stdout outside local, with time, level, message and
// service, the request's requestId and organizationId and the span's traceId and spanId, then the
// attributes, sensitive ones written as [REDACTED] (contracts/vectors/logging.json). Readable,
// still redacted, text locally. LOG_LEVEL sets the minimum.
package logging

import (
	"io"
	"log/slog"
	"os"
	"strings"
	"time"
)

// New returns the service's logger. local chooses text over JSON.
func New(w io.Writer, service string, local bool) *slog.Logger {
	level := levelFrom(os.Getenv("LOG_LEVEL"))
	if local {
		text := slog.NewTextHandler(w, &slog.HandlerOptions{Level: level, ReplaceAttr: redactAttr})
		return slog.New(localHandler{text}).With("service", service)
	}
	return slog.New(newHandler(w, service, level, nil))
}

func newHandler(w io.Writer, service string, level slog.Leveler, now func() time.Time) *handler {
	return &handler{out: &output{w: w}, level: level, service: service, now: now}
}

// levelFrom is LOG_LEVEL's minimum: debug, info (the default and anything unknown), warn or error.
func levelFrom(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}

// levelName is the line's level: slog levels between the named ones round down.
func levelName(l slog.Level) string {
	switch {
	case l < slog.LevelInfo:
		return "debug"
	case l < slog.LevelWarn:
		return "info"
	case l < slog.LevelError:
		return "warn"
	}
	return "error"
}
