package ratelimit

import (
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
)

// MetricName is the fail-open counter: ratelimit_fail_open_total{reason}.
const MetricName = "ratelimit_fail_open_total"

// Why a request went ahead unchecked: no Redis configured, Redis not answering in time (or at
// all), or an answer that isn't the script's. ReasonDebit isn't a request: it's a Debit Redis
// didn't take (for any of those reasons), after a request already served; Total doesn't count it.
const (
	ReasonUnconfigured = "unconfigured"
	ReasonUnavailable  = "unavailable"
	ReasonError        = "error"
	ReasonDebit        = "debit"
)

// LogEvery is how often a stretch of fail-opens is logged after its first.
const LogEvery = 100

// FailOpens counts every request allowed without an answer from Redis, by reason, and logs at a
// paced rate, as the kit's quota fail-opens do: the first of a stretch, then every 100th, then one
// line when Redis answers again (contracts/api-conventions.md, "Rate limits": counted and logged).
type FailOpens struct {
	Logger *slog.Logger

	mu      sync.Mutex
	counts  map[string]uint64
	total   uint64
	failing bool
}

// RecordDebit counts one debit Redis didn't take (Limiter.Debit), under ReasonDebit, logging the
// first and every LogEvery-th. It isn't a request allowed unchecked, so Total leaves it out.
func (f *FailOpens) RecordDebit(err error) {
	f.mu.Lock()
	if f.counts == nil {
		f.counts = map[string]uint64{}
	}
	f.counts[ReasonDebit]++
	n := f.counts[ReasonDebit]
	f.mu.Unlock()
	if f.Logger == nil || (n != 1 && n%LogEvery != 0) {
		return
	}
	args := []any{"metric", MetricName, "reason", ReasonDebit, "debits", n}
	if err != nil {
		args = append(args, "error", err.Error())
	}
	f.Logger.Warn("rate-limit Redis didn't take a debit after the request; the bucket wasn't charged (fail open)", args...)
}

// Record counts one request that failed open.
func (f *FailOpens) Record(reason string, err error) {
	f.mu.Lock()
	if f.counts == nil {
		f.counts = map[string]uint64{}
	}
	f.counts[reason]++
	f.total++
	started := !f.failing
	f.failing = true
	total := f.total
	f.mu.Unlock()
	if f.Logger == nil || !(started || total%LogEvery == 0) {
		return
	}
	args := []any{"metric", MetricName, "reason", reason, "total", total}
	if err != nil {
		args = append(args, "error", err.Error())
	}
	f.Logger.Warn("rate-limit Redis didn't answer; allowing the request unchecked (fail open)", args...)
}

// Answered notes that Redis answered, logging the end of a stretch of fail-opens.
func (f *FailOpens) Answered() {
	f.mu.Lock()
	ended := f.failing
	f.failing = false
	total := f.total
	f.mu.Unlock()
	if ended && f.Logger != nil {
		f.Logger.Info("rate-limit Redis is answering again", "metric", MetricName, "total", total)
	}
}

// Total is how many requests have failed open since the process started (debits not included).
func (f *FailOpens) Total() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.total
}

// Count is how many requests have failed open for reason.
func (f *FailOpens) Count(reason string) uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts[reason]
}

// WritePrometheus writes the counter in Prometheus' text format, for a service's metrics port.
func (f *FailOpens) WritePrometheus(w io.Writer) error {
	f.mu.Lock()
	reasons := make([]string, 0, len(f.counts))
	for reason := range f.counts {
		reasons = append(reasons, reason)
	}
	slices.Sort(reasons)
	lines := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		lines = append(lines, fmt.Sprintf("%s{reason=%q} %d\n", MetricName, reason, f.counts[reason]))
	}
	f.mu.Unlock()
	if _, err := fmt.Fprintf(w, "# HELP %s Requests allowed without an answer from the rate-limit Redis; reason debit: debits it didn't take.\n# TYPE %s counter\n", MetricName, MetricName); err != nil {
		return err
	}
	_, err := io.WriteString(w, strings.Join(lines, ""))
	return err
}
