// Package usage is the producer side of metering (contracts/metering.md): the usage event, its
// deterministic ID, hour splitting, the meter registry and validation. Package metering writes
// events to the outbox and relays them.
package usage

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/canonical"
	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/decimal"
)

// usageTime is what usage events accept: UTC with Z and zero to three fractional digits, never
// an offset.
var usageTime = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.(\d{1,3}))?Z$`)

// ParseTime reads a usage timestamp, refusing impossible instants (02-30, 24:00, 10:60) that the
// pattern alone would let through.
func ParseTime(s string) (time.Time, bool) {
	m := usageTime.FindStringSubmatch(s)
	if m == nil {
		return time.Time{}, false
	}
	n := make([]int, 6)
	for i := range n {
		n[i], _ = strconv.Atoi(m[i+1])
	}
	ms := 0
	if m[7] != "" {
		ms, _ = strconv.Atoi(m[7] + strings.Repeat("0", 3-len(m[7])))
	}
	t := time.Date(n[0], time.Month(n[1]), n[2], n[3], n[4], n[5], ms*int(time.Millisecond), time.UTC)
	// time.Date normalizes 02-30 into March and 10:60 into 11:00; a real instant round-trips.
	if t.Year() != n[0] || int(t.Month()) != n[1] || t.Day() != n[2] || t.Hour() != n[3] || t.Minute() != n[4] || t.Second() != n[5] {
		return time.Time{}, false
	}
	return t, true
}

// IDTime writes an instant as usage events and their IDs do: UTC with Z, milliseconds only when
// they're non-zero, and then always three digits (10:00:00Z, 10:42:17.250Z). One function for
// both, so a regenerated event is byte-identical to the first.
func IDTime(t time.Time) string {
	t = t.UTC().Truncate(time.Millisecond)
	if t.Nanosecond() == 0 {
		return t.Format("2006-01-02T15:04:05Z")
	}
	return t.Format("2006-01-02T15:04:05.000Z")
}

// IDTimeOf normalizes a usage timestamp string (10:42:17.25Z → 10:42:17.250Z).
func IDTimeOf(s string) (string, error) {
	t, ok := ParseTime(s)
	if !ok {
		return "", fmt.Errorf("usage: %q isn't a usage timestamp", s)
	}
	return IDTime(t), nil
}

// ResourceID is the last slug of a usage resource name (wgt_4k2m9x0a7qpe).
func ResourceID(resourceName string) (string, error) {
	parsed, ok := ParseResourceName(resourceName)
	if !ok {
		return "", fmt.Errorf("usage: %q isn't a usage resource name", resourceName)
	}
	return parsed.Chain[len(parsed.Chain)-1], nil
}

// IntervalID is <resource id>:<meter>:<window start>. A resource's intervals for one meter never
// overlap and every change closes one, so the window start is unique.
func IntervalID(resourceName, meter string, windowStart time.Time) (string, error) {
	rid, err := ResourceID(resourceName)
	if err != nil {
		return "", err
	}
	return rid + ":" + meter + ":" + IDTime(windowStart), nil
}

// AttributedID is the delta and gauge form:
// <resource id>:<meter>:<project id or "">:<hash16(dimensions)>:<hash16(tags)>:<time>. A delta
// window or gauge sample can coincide with another under a different project, dimensions or
// tags, so those are in the ID.
func AttributedID(resourceName, meter string, projectID *string, dimensions, tags map[string]string, at time.Time) (string, error) {
	rid, err := ResourceID(resourceName)
	if err != nil {
		return "", err
	}
	project := ""
	if projectID != nil {
		project = *projectID
	}
	return strings.Join([]string{rid, meter, project, canonical.Hash16Strings(dimensions), canonical.Hash16Strings(tags), IDTime(at)}, ":"), nil
}

// DeltaID is AttributedID at the window start (or the instant).
func DeltaID(resourceName, meter string, projectID *string, dimensions, tags map[string]string, windowStart time.Time) (string, error) {
	return AttributedID(resourceName, meter, projectID, dimensions, tags, windowStart)
}

// GaugeID is AttributedID at the sample's instant.
func GaugeID(resourceName, meter string, projectID *string, dimensions, tags map[string]string, occurredAt time.Time) (string, error) {
	return AttributedID(resourceName, meter, projectID, dimensions, tags, occurredAt)
}

// CorrectionID is <original id>:c<n>, n counting the original's corrections from 1.
func CorrectionID(originalID string, n int) (string, error) {
	if n < 1 {
		return "", fmt.Errorf("usage: corrections count from 1, got %d", n)
	}
	return originalID + ":c" + strconv.Itoa(n), nil
}

// Window is one interval inside a UTC hour.
type Window struct {
	Start, End time.Time
	// Quantity is the window's length in seconds, exact to the millisecond.
	Quantity decimal.Decimal
}

// SplitAtHours cuts [start, end) at every UTC hour boundary: a close or checkpoint that spans
// boundaries (the job ran late, the service was down for five hours) becomes one interval per
// hour touched, each with its own ID. Nothing is lost and nothing doubles, however late the job
// runs. Times are truncated to the millisecond first.
func SplitAtHours(start, end time.Time) []Window {
	start = start.UTC().Truncate(time.Millisecond)
	end = end.UTC().Truncate(time.Millisecond)
	var out []Window
	for at := start; at.Before(end); {
		next := at.Truncate(time.Hour).Add(time.Hour)
		if next.After(end) {
			next = end
		}
		out = append(out, Window{Start: at, End: next, Quantity: decimal.SecondsFromMillis(next.Sub(at).Milliseconds())})
		at = next
	}
	return out
}
