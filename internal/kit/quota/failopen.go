package quota

import (
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
)

// FailOpens counts every time a request went ahead without an answer from Quotas, per quota and
// reason: quotas_fail_open_total{quota, reason} (contracts/quotas.md, "Failing open"). It logs
// at a paced rate, as audit-stack's FailOpen does: the first fail-open, then every 100th, then one
// line when Quotas answers again. A 400 or 403 from Quotas is a bug (the manifest and the
// registration disagree), logged at error level every time. A 401 is the service's own qk_
// credential being refused (missing, expired, revoked or rotated away): logged at error level
// every time too, and also counted in quotas_credential_rejected_total, so an alert can tell a
// credential that needs rotating from Quotas being down.
type FailOpens struct {
	Logger *slog.Logger

	mu                 sync.Mutex
	counts             map[failOpenKey]uint64
	total              uint64
	credentialRejected uint64
	failing            bool
}

type failOpenKey struct{ quota, reason string }

// LogEvery is how often a stretch of fail-opens is logged after its first.
const LogEvery = 100

// MetricName is the counter's name.
const MetricName = "quotas_fail_open_total"

// CredentialRejectedMetricName is the counter of fail-opens whose cause was quotas-api answering
// 401 to the service's own credential: a subset of MetricName's requests.
const CredentialRejectedMetricName = "quotas_credential_rejected_total"

// Record counts one request that failed open, once for each of its quotas.
func (f *FailOpens) Record(quotas []string, failure *Failure) {
	f.mu.Lock()
	if f.counts == nil {
		f.counts = map[failOpenKey]uint64{}
	}
	for _, q := range quotas {
		f.counts[failOpenKey{q, failure.Reason}]++
	}
	f.total++
	rejected := failure.Status == 401
	if rejected {
		f.credentialRejected++
	}
	started := !f.failing
	f.failing = true
	total := f.total
	f.mu.Unlock()

	if f.Logger == nil {
		return
	}
	args := []any{"metric", MetricName, "reason", failure.Reason, "quotas", strings.Join(quotas, ","), "total", total, "error", failure.Error()}
	if rejected {
		f.Logger.Error("quotas-api refused this service's credential (QUOTAS_API_CREDENTIAL is missing, expired, revoked or rotated); allowing the request (fail open)",
			append(args, "alert", CredentialRejectedMetricName)...)
		return
	}
	if failure.Status == 400 || failure.Status == 403 {
		f.Logger.Error("quotas-api refused the request: the manifest and the registration disagree; allowing it (fail open)", args...)
		return
	}
	if started || total%LogEvery == 0 {
		f.Logger.Warn("quotas-api didn't answer; allowing the request as if unlimited (fail open)", args...)
	}
}

// Answered notes that Quotas answered, logging the end of a stretch of fail-opens.
func (f *FailOpens) Answered() {
	f.mu.Lock()
	ended := f.failing
	f.failing = false
	total := f.total
	f.mu.Unlock()
	if ended && f.Logger != nil {
		f.Logger.Info("quotas-api is answering again", "metric", MetricName, "total", total)
	}
}

// CredentialRejected is how many requests have failed open since the process started because
// quotas-api answered 401 to the service's credential.
func (f *FailOpens) CredentialRejected() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.credentialRejected
}

// Count is one series of the counter.
type Count struct {
	Quota  string
	Reason string
	Value  uint64
}

// Counts is every series, sorted.
func (f *FailOpens) Counts() []Count {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Count, 0, len(f.counts))
	for k, v := range f.counts {
		out = append(out, Count{Quota: k.quota, Reason: k.reason, Value: v})
	}
	slices.SortFunc(out, func(a, b Count) int { return strings.Compare(a.Quota+"\x00"+a.Reason, b.Quota+"\x00"+b.Reason) })
	return out
}

// Total is how many requests have failed open since the process started.
func (f *FailOpens) Total() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.total
}

// WritePrometheus writes the counter in Prometheus' text format, for a service that exposes
// metrics.
func (f *FailOpens) WritePrometheus(w io.Writer) error {
	if _, err := fmt.Fprintf(w, "# HELP %s Requests that went ahead without an answer from Quotas, per quota.\n# TYPE %s counter\n", MetricName, MetricName); err != nil {
		return err
	}
	for _, c := range f.Counts() {
		if _, err := fmt.Fprintf(w, "%s{quota=%q,reason=%q} %d\n", MetricName, c.Quota, c.Reason, c.Value); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(w, "# HELP %s Requests that failed open because quotas-api refused the service's credential (401).\n# TYPE %s counter\n%s %d\n",
		CredentialRejectedMetricName, CredentialRejectedMetricName, CredentialRejectedMetricName, f.CredentialRejected())
	return err
}
