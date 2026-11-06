// Package push sends a process's Prometheus metrics to Obs (the network-stack managed load balancer
// design's NL23), for a process nothing scrapes. It's the metrics-only exception to the no-exporter
// rule in docs/logging-design.md: traces stay unexported.
//
// It's a package of its own, apart from telemetry, which every service links. Its OTLP protos are
// the slim module's, and a binary that links both them and go.opentelemetry.io/proto/otlp (obs-stack
// does) panics at start on a duplicate protobuf registration. shortcut: a stack on the other module
// can't import this package; move to one module if one ever needs to.
//
// Name metrics for what Obs shows: it translates OTLP names at read as Prometheus's own receiver
// does. Gauges and histograms keep their names. A counter should end _total and have no other
// "total" word: lb_stages comes back lb_stages_total, and lb_total_bytes_total comes back
// lb_bytes_total, which a rule on the registry's name would never match.
package push

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	colmetricspb "go.opentelemetry.io/proto/slim/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/slim/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/slim/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/slim/otlp/resource/v1"
	"google.golang.org/protobuf/proto"
)

const (
	defaultInterval = 30 * time.Second
	timeout         = 10 * time.Second
	maxRetryAfter   = 300 * time.Second
	cumulative      = metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE
)

// Options configures Metrics. Everything is required except Interval (30 s) and Client.
type Options struct {
	// URL is obs-ingest's internal base URL (OBS_INGEST_INTERNAL_URL).
	URL string
	// Credential is this process's own ob_ service credential (OBS_INGEST_CREDENTIAL).
	Credential string
	// Organization and Project are the receiving project, as UUIDs (obs-ingest's scope headers).
	Organization, Project string
	// Service is the resources' service.name.
	Service string
	// Heartbeat names a gauge pushed at 1 every time. Nothing scrapes a pushing process, so there's
	// no up series, and a rule alerts on this one's absence.
	Heartbeat string
	Gatherer  prometheus.Gatherer
	Interval  time.Duration
	Client    *http.Client
	Logger    *slog.Logger
}

// Metrics posts Gatherer's metrics to obs-ingest's internal producer route (OTLP/HTTP protobuf,
// gzip, cumulative) every Interval, from a random start offset, until ctx ends, then once more. A
// failed push is dropped, not queued: counters are cumulative, so the next push carries what was
// lost. Each point has a pod label, the host name, because replicas push apart. It returns an
// error only when it can't start: incomplete options, or no host name.
func Metrics(ctx context.Context, o Options) error {
	if o.URL == "" || o.Credential == "" || o.Organization == "" || o.Project == "" || o.Service == "" || o.Heartbeat == "" || o.Gatherer == nil || o.Logger == nil {
		return errors.New("push.Metrics: URL, Credential, Organization, Project, Service, Heartbeat, Gatherer and Logger are required")
	}
	if o.Interval <= 0 {
		o.Interval = defaultInterval
	}
	if o.Client == nil {
		o.Client = &http.Client{Timeout: timeout}
	}
	pod, err := os.Hostname()
	if err != nil {
		return fmt.Errorf("push.Metrics: %w", err)
	}
	start := uint64(time.Now().UnixNano())

	timer := time.NewTimer(rand.N(o.Interval))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			// The final push outlives ctx, which is why it's over.
			final, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
			defer cancel()
			o.send(final, pod, start)
			return nil
		case <-timer.C:
			if ctx.Err() == nil { // select picks at random when both are ready
				timer.Reset(max(o.Interval, o.send(ctx, pod, start)))
			}
		}
	}
}

// send sends the current totals, and returns how long obs-ingest asked it to stay away.
func (o Options) send(ctx context.Context, pod string, start uint64) time.Duration {
	families, err := o.Gatherer.Gather()
	if err != nil { // what it did gather is still good
		o.Logger.WarnContext(ctx, "gathering metrics failed", "error", err.Error())
	}
	retryAfter, err := o.post(ctx, o.request(families, pod, start))
	if err != nil {
		o.Logger.WarnContext(ctx, "pushing metrics to Obs failed; dropped", "error", err.Error())
	}
	return retryAfter
}

func (o Options) post(ctx context.Context, m *colmetricspb.ExportMetricsServiceRequest) (time.Duration, error) {
	raw, err := proto.Marshal(m)
	if err != nil {
		return 0, err
	}
	var body bytes.Buffer
	zw := gzip.NewWriter(&body)
	if _, err := zw.Write(raw); err != nil {
		return 0, err
	}
	if err := zw.Close(); err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(o.URL, "/")+"/internal/v1/otlp/v1/metrics", &body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("Authorization", "Bearer "+o.Credential)
	req.Header.Set("x-ezgh-organization", o.Organization)
	req.Header.Set("x-ezgh-project", o.Project)
	resp, err := o.Client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		// A 200 can still refuse points, even all of them. The heartbeat can't show that, so say it.
		var r colmetricspb.ExportMetricsServiceResponse
		if proto.Unmarshal(answer, &r) == nil && r.GetPartialSuccess() != nil {
			if ps := r.GetPartialSuccess(); ps.GetRejectedDataPoints() > 0 || ps.GetErrorMessage() != "" {
				return 0, fmt.Errorf("obs-ingest rejected %d data points: %s", ps.GetRejectedDataPoints(), ps.GetErrorMessage())
			}
		}
		return 0, nil
	}
	var retryAfter time.Duration
	if n, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && n > 0 {
		retryAfter = min(time.Duration(n)*time.Second, maxRetryAfter)
	}
	return retryAfter, fmt.Errorf("obs-ingest answered %d", resp.StatusCode)
}

// Encode is families as one OTLP metrics request in protobuf, cumulative since start, for a
// process that sends its metrics its own way (dbagent, over its stream). Unlike Metrics it adds no
// heartbeat and no pod label or service.instance.id: the host name names a node, which mustn't
// reach a customer's series.
func Encode(families []*dto.MetricFamily, service string, start time.Time) ([]byte, error) {
	return proto.Marshal(request(families, service, "", uint64(start.UnixNano())))
}

func (o Options) request(families []*dto.MetricFamily, pod string, start uint64) *colmetricspb.ExportMetricsServiceRequest {
	r := request(families, o.Service, pod, start)
	sm := r.ResourceMetrics[0].ScopeMetrics[0]
	sm.Metrics = append(sm.Metrics, &metricspb.Metric{Name: o.Heartbeat, Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{
		DataPoints: []*metricspb.NumberDataPoint{number(1, []*commonpb.KeyValue{kv("pod", pod)}, start, uint64(time.Now().UnixNano()))}}}})
	return r
}

// request is families in OTLP; pod, when not "", labels every point and is service.instance.id.
func request(families []*dto.MetricFamily, service, pod string, start uint64) *colmetricspb.ExportMetricsServiceRequest {
	now := uint64(time.Now().UnixNano())
	metrics := make([]*metricspb.Metric, 0, len(families)+1)
	for _, f := range families {
		if m := otlpMetric(f, pod, start, now); m != nil {
			metrics = append(metrics, m)
		}
	}
	// obs-ingest routes by environment; these services run only in production.
	res := []*commonpb.KeyValue{kv("service.name", service), kv("ezgh.metadata.environment", "production")}
	if pod != "" {
		res = append(res, kv("service.instance.id", pod))
	}
	return &colmetricspb.ExportMetricsServiceRequest{ResourceMetrics: []*metricspb.ResourceMetrics{{
		Resource:     &resourcepb.Resource{Attributes: res},
		ScopeMetrics: []*metricspb.ScopeMetrics{{Scope: &commonpb.InstrumentationScope{Name: service}, Metrics: metrics}},
	}}}
}

// otlpMetric is one family in OTLP, with its Prometheus name and no unit, so Obs's read-time
// translation adds no unit suffix. Counters, histograms and summaries are cumulative since start.
// Nil for a type Gather's registries don't produce.
func otlpMetric(f *dto.MetricFamily, pod string, start, now uint64) *metricspb.Metric {
	m := &metricspb.Metric{Name: f.GetName(), Description: f.GetHelp()}
	switch f.GetType() {
	case dto.MetricType_COUNTER:
		points := make([]*metricspb.NumberDataPoint, 0, len(f.GetMetric()))
		for _, s := range f.GetMetric() {
			points = append(points, number(s.GetCounter().GetValue(), labels(s, pod), start, now))
		}
		m.Data = &metricspb.Metric_Sum{Sum: &metricspb.Sum{AggregationTemporality: cumulative, IsMonotonic: true, DataPoints: points}}
	case dto.MetricType_GAUGE, dto.MetricType_UNTYPED:
		points := make([]*metricspb.NumberDataPoint, 0, len(f.GetMetric()))
		for _, s := range f.GetMetric() {
			v := s.GetGauge().GetValue()
			if f.GetType() == dto.MetricType_UNTYPED {
				v = s.GetUntyped().GetValue()
			}
			points = append(points, number(v, labels(s, pod), start, now))
		}
		m.Data = &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{DataPoints: points}}
	case dto.MetricType_HISTOGRAM:
		points := make([]*metricspb.HistogramDataPoint, 0, len(f.GetMetric()))
		for _, s := range f.GetMetric() {
			points = append(points, histogramPoint(s.GetHistogram(), labels(s, pod), start, now))
		}
		m.Data = &metricspb.Metric_Histogram{Histogram: &metricspb.Histogram{AggregationTemporality: cumulative, DataPoints: points}}
	case dto.MetricType_SUMMARY:
		points := make([]*metricspb.SummaryDataPoint, 0, len(f.GetMetric()))
		for _, s := range f.GetMetric() {
			quantiles := make([]*metricspb.SummaryDataPoint_ValueAtQuantile, 0, len(s.GetSummary().GetQuantile()))
			for _, q := range s.GetSummary().GetQuantile() {
				quantiles = append(quantiles, &metricspb.SummaryDataPoint_ValueAtQuantile{Quantile: q.GetQuantile(), Value: q.GetValue()})
			}
			points = append(points, &metricspb.SummaryDataPoint{Attributes: labels(s, pod), StartTimeUnixNano: start, TimeUnixNano: now,
				Count: s.GetSummary().GetSampleCount(), Sum: s.GetSummary().GetSampleSum(), QuantileValues: quantiles})
		}
		m.Data = &metricspb.Metric_Summary{Summary: &metricspb.Summary{DataPoints: points}}
	default:
		return nil
	}
	return m
}

// histogramPoint turns Prometheus's cumulative buckets into OTLP's per-bucket counts, the last
// being the overflow. Prometheus's +Inf bucket, when present, is that overflow, not a bound.
func histogramPoint(h *dto.Histogram, attrs []*commonpb.KeyValue, start, now uint64) *metricspb.HistogramDataPoint {
	var bounds []float64
	var counts []uint64
	var seen uint64
	for _, b := range h.GetBucket() {
		if math.IsInf(b.GetUpperBound(), 1) {
			break
		}
		bounds = append(bounds, b.GetUpperBound())
		counts = append(counts, b.GetCumulativeCount()-seen)
		seen = b.GetCumulativeCount()
	}
	sum := h.GetSampleSum()
	return &metricspb.HistogramDataPoint{Attributes: attrs, StartTimeUnixNano: start, TimeUnixNano: now, Count: h.GetSampleCount(),
		Sum: &sum, ExplicitBounds: bounds, BucketCounts: append(counts, h.GetSampleCount()-seen)}
}

func number(v float64, attrs []*commonpb.KeyValue, start, now uint64) *metricspb.NumberDataPoint {
	return &metricspb.NumberDataPoint{Attributes: attrs, StartTimeUnixNano: start, TimeUnixNano: now, Value: &metricspb.NumberDataPoint_AsDouble{AsDouble: v}}
}

// labels are a sample's labels, plus pod unless it has its own or pod is "".
func labels(s *dto.Metric, pod string) []*commonpb.KeyValue {
	out := make([]*commonpb.KeyValue, 0, len(s.GetLabel())+1)
	hasPod := pod == ""
	for _, l := range s.GetLabel() {
		hasPod = hasPod || l.GetName() == "pod"
		out = append(out, kv(l.GetName(), l.GetValue()))
	}
	if !hasPod {
		out = append(out, kv("pod", pod))
	}
	return out
}

func kv(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}
