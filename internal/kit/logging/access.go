package logging

import (
	"log/slog"
	"math"
	"regexp"
	"strings"
)

// Access is what the access middleware saw (contracts/vectors/logging.json, access).
type Access struct {
	Method        string
	Route         string
	Status        int
	DurationMs    float64
	RequestBytes  int64 // -1 when unknown
	ResponseBytes int64 // -1 when unknown
	ClientIP      string
	UserAgent     string
	Caller        Caller
	ErrorCode     string
}

// AccessAttrs is the access line's level and attributes; the logger adds the base and context
// fields. info below 500, error from 500.
func AccessAttrs(a Access) (slog.Level, []slog.Attr) {
	attrs := []slog.Attr{
		slog.String("kind", "access"),
		slog.String("method", a.Method),
		slog.String("route", a.Route),
		slog.Int("status", a.Status),
		slog.Float64("durationMs", math.Round(a.DurationMs*1000)/1000),
	}
	if a.RequestBytes >= 0 {
		attrs = append(attrs, slog.Int64("requestBytes", a.RequestBytes))
	}
	if a.ResponseBytes >= 0 {
		attrs = append(attrs, slog.Int64("responseBytes", a.ResponseBytes))
	}
	if a.ClientIP != "" {
		attrs = append(attrs, slog.String("clientIp", a.ClientIP))
	}
	if ua := truncate(a.UserAgent, 256); ua != "" {
		attrs = append(attrs, slog.String("userAgent", ua))
	}
	if a.Caller.Type != "" {
		attrs = append(attrs, slog.String("principalType", a.Caller.Type))
		if a.Caller.ID != "" {
			attrs = append(attrs, slog.String("principalId", a.Caller.ID))
		}
		if a.Caller.APIKeyID != "" {
			attrs = append(attrs, slog.String("apiKeyId", a.Caller.APIKeyID))
		}
	}
	if a.ErrorCode != "" {
		attrs = append(attrs, slog.String("errorCode", a.ErrorCode))
	}
	if a.Status >= 500 {
		return slog.LevelError, attrs
	}
	return slog.LevelInfo, attrs
}

// truncate keeps the first n code points.
func truncate(s string, n int) string {
	i := 0
	for j := range s {
		if i == n {
			return s[:j]
		}
		i++
	}
	return s
}

var colonParam = regexp.MustCompile(`/:([A-Za-z_][A-Za-z0-9_]*)`)

// RouteOf is a router's pattern as the access line's route: no method, parameters as {name}
// (":name" from Effect and Hono rewritten), "unmatched" when there's none.
func RouteOf(pattern string) string {
	if _, path, ok := strings.Cut(pattern, " "); ok {
		pattern = path
	}
	if pattern == "" {
		return "unmatched"
	}
	return colonParam.ReplaceAllString(pattern, "/{$1}")
}
