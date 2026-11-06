// Package instant reads and writes timestamps the way the platform's APIs do
// (contracts/api-conventions.md, "Timestamps, decimals and money").
//
// Responses always carry RFC 3339 UTC with exactly three fractional digits, as ECMAScript's
// toISOString writes them, so clients in every language parse one shape. Requests may send any
// RFC 3339 timestamp with Z or an offset, to the millisecond at most. Usage events have a third
// form of their own; see package usage.
package instant

import (
	"regexp"
	"time"
)

// Layout is the response form: 2026-09-28T10:42:17.250Z.
const Layout = "2006-01-02T15:04:05.000Z"

var responseForm = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$`)

// Format writes t in the response form, in UTC, truncated to the millisecond.
func Format(t time.Time) string { return t.UTC().Truncate(time.Millisecond).Format(Layout) }

// FormatPtr is Format for optional instants: nil stays nil (JSON null).
func FormatPtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := Format(*t)
	return &s
}

// FromMillis is the instant ms milliseconds after the Unix epoch.
func FromMillis(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

// ParseResponse reads a timestamp in exactly the response form, refusing impossible dates
// (2026-02-30) that the pattern alone would let through.
func ParseResponse(s string) (time.Time, bool) {
	if !responseForm.MatchString(s) {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

var requestForm = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d{1,3})?(Z|[+-]\d{2}:\d{2})$`)

// ParseRequest reads a timestamp a client sent: RFC 3339 with Z or an offset, and at most
// millisecond precision. It's returned in UTC.
func ParseRequest(s string) (time.Time, bool) {
	if !requestForm.MatchString(s) {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

// TruncateHour is the start of t's UTC hour.
func TruncateHour(t time.Time) time.Time { return t.UTC().Truncate(time.Hour) }
