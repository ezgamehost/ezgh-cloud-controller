package push_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	colmetricspb "go.opentelemetry.io/proto/slim/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/slim/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/slim/otlp/metrics/v1"
	"google.golang.org/protobuf/proto"

	"github.com/ezgamehost/ezgh-cloud-controller/internal/kit/telemetry/push"
)

const (
	testCredential = "ob_3k9d0a1m2x7q4b8c.AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"
	testOrg        = "0199a3c2-5b1e-7d40-9f3a-2c8e6b1d4f70"
	testProject    = "0199a3c2-6c2f-7e51-8a4b-3d9f7c2e5a81"
)

// A received push.
type received struct {
	header http.Header
	path   string
	req    *colmetricspb.ExportMetricsServiceRequest
}

// sink is obs-ingest: it records every push and answers with status(n), n counting from 0.
type sink struct {
	*httptest.Server
	mu     sync.Mutex
	pushes []received
	got    chan struct{}
	status func(n int, w http.ResponseWriter) int
	reply  []byte
}

func newSink(t *testing.T) *sink {
	t.Helper()
	s := &sink{got: make(chan struct{}, 1024)}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Errorf("body isn't gzip: %v", err)
			return
		}
		raw, err := io.ReadAll(zr)
		if err != nil {
			t.Errorf("reading the body: %v", err)
			return
		}
		req := &colmetricspb.ExportMetricsServiceRequest{}
		if err := proto.Unmarshal(raw, req); err != nil {
			t.Errorf("body isn't OTLP: %v", err)
			return
		}
		s.mu.Lock()
		n := len(s.pushes)
		s.pushes = append(s.pushes, received{r.Header.Clone(), r.URL.Path, req})
		s.mu.Unlock()
		if s.status != nil {
			w.WriteHeader(s.status(n, w))
		}
		_, _ = w.Write(s.reply)
		s.got <- struct{}{}
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *sink) wait(t *testing.T, n int) []received {
	t.Helper()
	for range n {
		select {
		case <-s.got:
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for %d pushes", n)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.pushes)
}

func options(s *sink, g prometheus.Gatherer) push.Options {
	return push.Options{URL: s.URL + "/", Credential: testCredential, Organization: testOrg, Project: testProject,
		Service: "lb-controller", Heartbeat: "lb_push_heartbeat", Gatherer: g, Interval: time.Hour,
		Logger: slog.New(slog.DiscardHandler)}
}

// pushOnce runs Metrics with its context already over: with an hour's interval the only push
// is the final one.
func pushOnce(t *testing.T, o push.Options) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := push.Metrics(ctx, o); err != nil {
		t.Fatal(err)
	}
}

func metric(t *testing.T, p received, name string) *metricspb.Metric {
	t.Helper()
	for _, rm := range p.req.GetResourceMetrics() {
		for _, sm := range rm.GetScopeMetrics() {
			for _, m := range sm.GetMetrics() {
				if m.GetName() == name {
					return m
				}
			}
		}
	}
	t.Fatalf("no metric %q", name)
	return nil
}

func attrs(kvs []*commonpb.KeyValue) map[string]string {
	out := map[string]string{}
	for _, kv := range kvs {
		out[kv.GetKey()] = kv.GetValue().GetStringValue()
	}
	return out
}

func TestPushesCountersGaugesHistograms(t *testing.T) {
	reg := prometheus.NewRegistry()
	counter := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "lb_stages_total", Help: "Stages."}, []string{"result"})
	gauge := prometheus.NewGauge(prometheus.GaugeOpts{Name: "lb_nodes", Help: "Nodes."})
	hist := prometheus.NewHistogram(prometheus.HistogramOpts{Name: "lb_stage_seconds", Help: "Stage time.", Buckets: []float64{1, 5}})
	summary := prometheus.NewSummary(prometheus.SummaryOpts{Name: "lb_lag_seconds", Help: "Lag.", Objectives: map[float64]float64{0.5: 0.01}})
	reg.MustRegister(counter, gauge, hist, summary)
	counter.WithLabelValues("ok").Add(3)
	gauge.Set(2.5)
	for _, v := range []float64{0.5, 3, 3, 9} {
		hist.Observe(v)
	}
	summary.Observe(2)

	s := newSink(t)
	pushOnce(t, options(s, reg))
	p := s.wait(t, 1)[0]

	c := metric(t, p, "lb_stages_total")
	if c.GetSum() == nil || !c.GetSum().GetIsMonotonic() || c.GetSum().GetAggregationTemporality() != metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE {
		t.Fatalf("counter isn't a cumulative monotonic sum: %v", c)
	}
	dp := c.GetSum().GetDataPoints()[0]
	if dp.GetAsDouble() != 3 || attrs(dp.GetAttributes())["result"] != "ok" || c.GetDescription() != "Stages." {
		t.Fatalf("counter point: %v", c)
	}
	if dp.GetStartTimeUnixNano() == 0 || dp.GetStartTimeUnixNano() > dp.GetTimeUnixNano() {
		t.Fatalf("counter times: %v", dp)
	}
	if g := metric(t, p, "lb_nodes").GetGauge(); g == nil || g.GetDataPoints()[0].GetAsDouble() != 2.5 {
		t.Fatalf("gauge: %v", g)
	}
	h := metric(t, p, "lb_stage_seconds").GetHistogram()
	if h == nil || h.GetAggregationTemporality() != metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE {
		t.Fatalf("histogram: %v", h)
	}
	hp := h.GetDataPoints()[0]
	if !slices.Equal(hp.GetExplicitBounds(), []float64{1, 5}) || !slices.Equal(hp.GetBucketCounts(), []uint64{1, 2, 1}) {
		t.Fatalf("buckets: bounds %v, counts %v, want per-bucket counts", hp.GetExplicitBounds(), hp.GetBucketCounts())
	}
	if hp.GetCount() != 4 || hp.GetSum() != 15.5 {
		t.Fatalf("histogram count and sum: %v", hp)
	}
	sp := metric(t, p, "lb_lag_seconds").GetSummary().GetDataPoints()[0]
	if sp.GetCount() != 1 || sp.GetSum() != 2 || len(sp.GetQuantileValues()) != 1 || sp.GetQuantileValues()[0].GetValue() != 2 {
		t.Fatalf("summary: %v", sp)
	}
}

// Prometheus's +Inf bucket is OTLP's overflow, not a bound: obs-ingest refuses an infinite one.
func TestInfBucketIsTheOverflow(t *testing.T) {
	bucket := func(le float64, n uint64) *dto.Bucket { return &dto.Bucket{UpperBound: &le, CumulativeCount: &n} }
	count, sum, name, kind := uint64(3), 4.0, "lb_inf_seconds", dto.MetricType_HISTOGRAM
	g := prometheus.GathererFunc(func() ([]*dto.MetricFamily, error) {
		return []*dto.MetricFamily{{Name: &name, Type: &kind, Metric: []*dto.Metric{{Histogram: &dto.Histogram{
			SampleCount: &count, SampleSum: &sum, Bucket: []*dto.Bucket{bucket(1, 2), bucket(math.Inf(1), 3)}}}}}}, nil
	})
	s := newSink(t)
	pushOnce(t, options(s, g))
	hp := metric(t, s.wait(t, 1)[0], name).GetHistogram().GetDataPoints()[0]
	if !slices.Equal(hp.GetExplicitBounds(), []float64{1}) || !slices.Equal(hp.GetBucketCounts(), []uint64{2, 1}) {
		t.Fatalf("bounds %v, counts %v", hp.GetExplicitBounds(), hp.GetBucketCounts())
	}
}

func TestSendsScopeHeadersAndCredential(t *testing.T) {
	s := newSink(t)
	pushOnce(t, options(s, prometheus.NewRegistry()))
	p := s.wait(t, 1)[0]
	if p.path != "/internal/v1/otlp/v1/metrics" {
		t.Fatalf("path %q", p.path)
	}
	for k, want := range map[string]string{
		"Authorization": "Bearer " + testCredential, "X-Ezgh-Organization": testOrg, "X-Ezgh-Project": testProject,
		"Content-Type": "application/x-protobuf", "Content-Encoding": "gzip",
	} {
		if got := p.header.Get(k); got != want {
			t.Errorf("%s: %q, want %q", k, got, want)
		}
	}
	res := attrs(p.req.GetResourceMetrics()[0].GetResource().GetAttributes())
	if res["service.name"] != "lb-controller" || res["ezgh.metadata.environment"] != "production" {
		t.Fatalf("resource: %v", res)
	}
}

func TestPodLabelAdded(t *testing.T) {
	reg := prometheus.NewRegistry()
	mine := prometheus.NewGauge(prometheus.GaugeOpts{Name: "lb_plain", Help: "x"})
	theirs := prometheus.NewGauge(prometheus.GaugeOpts{Name: "lb_own_pod", Help: "x", ConstLabels: prometheus.Labels{"pod": "theirs"}})
	reg.MustRegister(mine, theirs)
	s := newSink(t)
	pushOnce(t, options(s, reg))
	p := s.wait(t, 1)[0]
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	if got := attrs(metric(t, p, "lb_plain").GetGauge().GetDataPoints()[0].GetAttributes())["pod"]; got != host {
		t.Fatalf("pod %q, want %q", got, host)
	}
	if got := attrs(p.req.GetResourceMetrics()[0].GetResource().GetAttributes())["service.instance.id"]; got != host {
		t.Fatalf("service.instance.id %q, want %q", got, host)
	}
	a := metric(t, p, "lb_own_pod").GetGauge().GetDataPoints()[0].GetAttributes()
	if len(a) != 1 || attrs(a)["pod"] != "theirs" {
		t.Fatalf("a metric's own pod label: %v", a)
	}
}

func TestRandomStartOffset(t *testing.T) {
	const interval = 400 * time.Millisecond
	s := newSink(t)
	var (
		mu    sync.Mutex
		wg    sync.WaitGroup
		first []time.Duration
		begin time.Time
		once  = make([]sync.Once, 16)
		opts  = make([]push.Options, len(once))
	)
	for i := range once {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			once[i].Do(func() {
				mu.Lock()
				first = append(first, time.Since(begin))
				mu.Unlock()
				wg.Done()
			})
		}))
		defer srv.Close()
		opts[i] = options(s, prometheus.NewRegistry())
		opts[i].URL, opts[i].Interval = srv.URL, interval
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	wg.Add(len(opts))
	begin = time.Now()
	for _, o := range opts {
		go func() { _ = push.Metrics(ctx, o) }()
	}
	wg.Wait()
	slices.Sort(first)
	if last := first[len(first)-1]; last > interval+time.Second {
		t.Fatalf("a first push came after %v, over one interval", last)
	}
	if spread := first[len(first)-1] - first[0]; spread < interval/10 {
		t.Fatalf("first pushes within %v of each other: no random offset", spread)
	}
}

func TestFailedPushIsDropped(t *testing.T) {
	reg := prometheus.NewRegistry()
	c := prometheus.NewCounter(prometheus.CounterOpts{Name: "lb_things_total", Help: "x"})
	reg.MustRegister(c)
	c.Add(1)
	s := newSink(t)
	failed := make(chan struct{})
	s.status = func(n int, _ http.ResponseWriter) int {
		if n == 0 {
			c.Add(4) // what the failed push never carried, and the next must
			close(failed)
			return http.StatusInternalServerError
		}
		return http.StatusOK
	}
	o := options(s, reg)
	o.Interval = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- push.Metrics(ctx, o) }()
	<-failed
	pushes := s.wait(t, 2)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	dps := metric(t, pushes[1], "lb_things_total").GetSum().GetDataPoints()
	if len(dps) != 1 || dps[0].GetAsDouble() != 5 {
		t.Fatalf("next push: %v, want the one current total", dps)
	}
}

func TestRetryAfterDelaysNextPush(t *testing.T) {
	s := newSink(t)
	var mu sync.Mutex
	var at []time.Time
	s.status = func(n int, w http.ResponseWriter) int {
		mu.Lock()
		defer mu.Unlock()
		at = append(at, time.Now())
		if n == 0 {
			w.Header().Set("Retry-After", "1")
			return http.StatusTooManyRequests
		}
		return http.StatusOK
	}
	o := options(s, prometheus.NewRegistry())
	o.Interval = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- push.Metrics(ctx, o) }()
	s.wait(t, 2)
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if gap := at[1].Sub(at[0]); gap < 900*time.Millisecond {
		t.Fatalf("next push after %v, Retry-After was 1 s", gap)
	}
}

func TestStopsOnContext(t *testing.T) {
	reg := prometheus.NewRegistry()
	g := prometheus.NewGauge(prometheus.GaugeOpts{Name: "lb_last", Help: "x"})
	reg.MustRegister(g)
	s := newSink(t)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- push.Metrics(ctx, options(s, reg)) }()
	g.Set(7)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Metrics didn't stop")
	}
	if got := metric(t, s.wait(t, 1)[0], "lb_last").GetGauge().GetDataPoints()[0].GetAsDouble(); got != 7 {
		t.Fatalf("final push: %v", got)
	}
}

func TestHeartbeatSeries(t *testing.T) {
	s := newSink(t)
	pushOnce(t, options(s, prometheus.NewRegistry()))
	g := metric(t, s.wait(t, 1)[0], "lb_push_heartbeat").GetGauge()
	if g == nil || g.GetDataPoints()[0].GetAsDouble() != 1 || attrs(g.GetDataPoints()[0].GetAttributes())["pod"] == "" {
		t.Fatalf("heartbeat: %v", g)
	}
}

func TestRejectedPointsAreLogged(t *testing.T) {
	reply, err := proto.Marshal(&colmetricspb.ExportMetricsServiceResponse{PartialSuccess: &colmetricspb.ExportMetricsPartialSuccess{
		RejectedDataPoints: 3, ErrorMessage: "the project's dataset limit is reached"}})
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	s := newSink(t)
	s.reply = reply
	o := options(s, prometheus.NewRegistry())
	o.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	pushOnce(t, o)
	s.wait(t, 1)
	if !strings.Contains(logs.String(), "rejected 3 data points") || !strings.Contains(logs.String(), "dataset limit") {
		t.Fatalf("logs: %q", logs.String())
	}

	logs.Reset()
	s.reply = nil
	pushOnce(t, o)
	s.wait(t, 1)
	if logs.Len() != 0 {
		t.Fatalf("a clean push logged: %q", logs.String())
	}
}

func TestCredentialNeverLogged(t *testing.T) {
	var logs bytes.Buffer
	s := newSink(t)
	s.status = func(int, http.ResponseWriter) int { return http.StatusUnauthorized }
	o := options(s, prometheus.NewRegistry())
	o.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	pushOnce(t, o)
	s.wait(t, 1)
	dead := httptest.NewServer(http.NotFoundHandler())
	o.URL = dead.URL
	dead.Close()
	pushOnce(t, o)
	if logs.Len() == 0 {
		t.Fatal("a failed push logged nothing")
	}
	if strings.Contains(logs.String(), "AAECAwQ") || strings.Contains(logs.String(), "ob_3k9d") {
		t.Fatalf("credential in the logs: %s", logs.String())
	}
}

func TestMetricsRefusesIncompleteOptions(t *testing.T) {
	full := options(newSink(t), prometheus.NewRegistry())
	for name, strip := range map[string]func(*push.Options){
		"URL":          func(o *push.Options) { o.URL = "" },
		"Credential":   func(o *push.Options) { o.Credential = "" },
		"Organization": func(o *push.Options) { o.Organization = "" },
		"Project":      func(o *push.Options) { o.Project = "" },
		"Service":      func(o *push.Options) { o.Service = "" },
		"Heartbeat":    func(o *push.Options) { o.Heartbeat = "" },
		"Gatherer":     func(o *push.Options) { o.Gatherer = nil },
		"Logger":       func(o *push.Options) { o.Logger = nil },
	} {
		o := full
		strip(&o)
		if err := push.Metrics(t.Context(), o); err == nil {
			t.Errorf("without %s: no error", name)
		}
	}
}

func TestEncodeHasNoHeartbeatAndNoPod(t *testing.T) {
	reg := prometheus.NewRegistry()
	c := prometheus.NewCounter(prometheus.CounterOpts{Name: "db_things_total", Help: "x", ConstLabels: prometheus.Labels{"database": "dbi_1"}})
	c.Add(3)
	reg.MustRegister(c)
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now().Add(-time.Minute)
	b, err := push.Encode(families, "dbagent", start)
	if err != nil {
		t.Fatal(err)
	}
	var req colmetricspb.ExportMetricsServiceRequest
	if err := proto.Unmarshal(b, &req); err != nil {
		t.Fatal(err)
	}
	host, _ := os.Hostname()
	if host != "" && bytes.Contains(b, []byte(host)) {
		t.Fatalf("the host name %q is in the payload", host)
	}
	rm := req.GetResourceMetrics()[0]
	if got := attrs(rm.GetResource().GetAttributes()); got["service.name"] != "dbagent" || got["service.instance.id"] != "" {
		t.Fatalf("resource %v", got)
	}
	ms := rm.GetScopeMetrics()[0].GetMetrics()
	if len(ms) != 1 || ms[0].GetName() != "db_things_total" {
		t.Fatalf("metrics %v, want db_things_total alone", ms)
	}
	p := ms[0].GetSum().GetDataPoints()[0]
	if a := attrs(p.GetAttributes()); len(a) != 1 || a["database"] != "dbi_1" || p.GetAsDouble() != 3 || !ms[0].GetSum().GetIsMonotonic() {
		t.Fatalf("point %v", p)
	}
	if p.GetStartTimeUnixNano() != uint64(start.UnixNano()) {
		t.Fatalf("start %d, want %d", p.GetStartTimeUnixNano(), start.UnixNano())
	}
}
