package telemetry

import (
	"bytes"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	apimetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry/onecollector"
)

func collectorFixture(t *testing.T, mappings []onecollector.Mapping, handler http.HandlerFunc) OneCollectorOptions {
	t.Helper()
	t.Setenv("SYNTHETIC_UPLOAD", "1")
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	return OneCollectorOptions{Profile: onecollector.Profile{ProfileVersion: 1, EnableEnv: "SYNTHETIC_UPLOAD", Endpoint: server.URL,
		RoutingKey: onecollector.KeyRef{Env: "SYNTHETIC_ROUTE"}, InstrumentationKey: onecollector.KeyRef{Env: "SYNTHETIC_KEY"}, Mappings: mappings},
		Options: onecollector.Options{EnableEnv: "SYNTHETIC_UPLOAD", Endpoint: server.URL, RoutingKey: "synthetic-route", InstrumentationKey: "synthetic-key"}, Transport: server.Client().Transport}
}

func syntheticMapping(signal string) onecollector.Mapping {
	m := onecollector.Mapping{Signal: signal, EventName: "Synthetic." + signal, EnvelopeVersion: "4.0", Fields: map[string]onecollector.Field{
		"body": {Source: signal + ".raw_json"}, "value": {Source: "metric.integer_value"}, "index": {Source: "metric.sample_index"}, "point": {Source: "metric.point_id"},
	}}
	if signal == "metric" {
		m.Match = map[string][]string{"metric.name": {"synthetic.counter", "synthetic.histogram"}, "metric.unit": {"1"}, "metric.kind": {"sum", "histogram"}}
	}
	return m
}

func TestOneCollectorJournalTraceMetricShutdown(t *testing.T) {
	var mu sync.Mutex
	var records []map[string]json.RawMessage
	cfg := collectorFixture(t, []onecollector.Mapping{syntheticMapping("journal"), syntheticMapping("trace"), syntheticMapping("metric")}, func(w http.ResponseWriter, r *http.Request) {
		decoder := json.NewDecoder(r.Body)
		mu.Lock()
		defer mu.Unlock()
		for decoder.More() {
			var envelope struct {
				Data map[string]json.RawMessage `json:"data"`
			}
			if err := decoder.Decode(&envelope); err != nil {
				t.Error(err)
				break
			}
			records = append(records, envelope.Data)
		}
		w.WriteHeader(200)
	})
	registry, scrubber := journal.DefaultScrubber()
	registry.Register([]byte("synthetic-exact-secret"))
	root := t.TempDir()
	client, err := New(t.Context(), Config{OneCollector: &cfg, NoDefaultExporter: true, Scrubber: scrubber, JournalRoot: root, MetricExportInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	committed := scrubber.Scrub([]byte(`{ "type":"synthetic", "number":9223372036854775807, "text":"synthetic-exact-secret" }`))
	registry.Register([]byte("9223372036854775807"))
	client.Commit(journal.CommittedEvent{Body: committed, Time: time.Now(), JournalID: "synthetic-journal"})
	ctx, span := client.tracer.Start(t.Context(), "synthetic-exact-secret")
	span.SetAttributes(attribute.String("sample", "synthetic-exact-secret"))
	span.End()
	counter, err := client.meterProvider.Meter("synthetic").Int64Counter("synthetic.counter", apimetric.WithUnit("1"))
	if err != nil {
		t.Fatal(err)
	}
	counter.Add(ctx, 9223372036854775807)
	if err := client.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(records) != 3 {
		t.Fatalf("records=%d expected journal, trace and final metric", len(records))
	}
	joined, _ := json.Marshal(records)
	if bytes.Contains(joined, []byte("synthetic-exact-secret")) {
		t.Fatal("configured scrubber leaked")
	}
	if !bytes.Contains(joined, []byte("9223372036854775807")) {
		t.Fatal("int64 precision lost")
	}
	foundScalar := false
	for _, record := range records {
		if string(record["value"]) == "9223372036854775807" {
			foundScalar = true
			if string(record["index"]) != `"0"` {
				t.Fatal("scalar sample index must be decimal zero")
			}
		}
	}
	if !foundScalar {
		t.Fatal("scalar record absent")
	}
	if journal.HasCommittedEventSink(root) {
		t.Fatal("sink not released")
	}
	if client.JournalLogsEnabled() {
		t.Fatal("closed collector still reports a live journal queue")
	}
}

func TestOneCollectorHistogramRowsAndContinueAfterBadMetric(t *testing.T) {
	var mu sync.Mutex
	var values, indices, ids []string
	cfg := collectorFixture(t, []onecollector.Mapping{syntheticMapping("metric")}, func(w http.ResponseWriter, r *http.Request) {
		decoder := json.NewDecoder(r.Body)
		mu.Lock()
		defer mu.Unlock()
		for decoder.More() {
			var envelope struct {
				Data map[string]json.RawMessage `json:"data"`
			}
			if err := decoder.Decode(&envelope); err != nil {
				t.Error(err)
				break
			}
			values = append(values, string(envelope.Data["value"]))
			indices = append(indices, string(envelope.Data["index"]))
			ids = append(ids, string(envelope.Data["point"]))
		}
		w.WriteHeader(200)
	})
	p := newOneCollectorPipeline(cfg, journal.NewPatternScrubber())
	exporter := collectorMetricExporter{pipeline: p}
	now := time.Now()
	data := &metricdata.ResourceMetrics{Resource: resource.Empty(), ScopeMetrics: []metricdata.ScopeMetrics{{Scope: instrumentation.Scope{Name: "synthetic"}, Metrics: []metricdata.Metrics{
		{Name: "synthetic.counter", Unit: "1", Data: metricdata.Sum[int64]{IsMonotonic: true, DataPoints: []metricdata.DataPoint[int64]{{Value: -1, Time: now}}}},
		{Name: "synthetic.counter", Unit: "1", Data: metricdata.Sum[float64]{DataPoints: []metricdata.DataPoint[float64]{{Value: 0x1p63, Time: now}}}},
		{Name: "synthetic.histogram", Unit: "1", Data: metricdata.Histogram[float64]{Temporality: metricdata.DeltaTemporality, DataPoints: []metricdata.HistogramDataPoint[float64]{{Time: now, Count: 3, Sum: 6, Min: metricdata.NewExtrema(1.0), Max: metricdata.NewExtrema(3.0), Bounds: []float64{1}, BucketCounts: []uint64{1, 2}}}}},
	}}}}
	if err := exporter.Export(t.Context(), data); err == nil {
		t.Fatal("negative monotonic sum accepted")
	}
	if err := p.shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(values) != 3 {
		t.Fatal(values)
	}
	for i, index := range indices {
		if index != strconv.Quote(strconv.Itoa(i)) {
			t.Fatal(indices)
		}
		if ids[i] != ids[0] {
			t.Fatal(ids)
		}
	}
}

func TestOneCollectorQueueBoundsAndFailureRelease(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	cfg := collectorFixture(t, []onecollector.Mapping{syntheticMapping("journal")}, func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(started) })
		<-release
		w.WriteHeader(503)
	})
	p := newOneCollectorPipeline(cfg, journal.NewPatternScrubber())
	raw := []byte(`{"type":"synthetic","body":"` + strings.Repeat("x", 900<<10) + `"}`)
	p.offer(collectorItem{signal: "journal", raw: raw})
	<-started
	for range 100 {
		p.offer(collectorItem{signal: "journal", raw: raw})
	}
	p.mu.Lock()
	if p.bytes > oneCollectorQueueBytes || p.count > oneCollectorQueueCount {
		t.Fatal("unbounded queue")
	}
	p.mu.Unlock()
	if p.dropped.Load() == 0 {
		t.Fatal("overload not counted")
	}
	close(release)
	if err := p.shutdown(t.Context()); err == nil {
		t.Fatal("HTTP failure not reported")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.bytes != 0 || p.count != 0 {
		t.Fatal("reservations leaked", p.bytes, p.count)
	}
}

func TestOneCollectorOversizedSpanDoesNotDropLaterSpan(t *testing.T) {
	calls := 0
	cfg := collectorFixture(t, []onecollector.Mapping{syntheticMapping("trace")}, func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(200) })
	p := newOneCollectorPipeline(cfg, journal.NewPatternScrubber())
	processor := collectorSpanProcessor{p}
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(processor))
	large := tracetest.SpanStub{Name: strings.Repeat("x", onecollector.MaxNativeBytes+1), StartTime: time.Now(), EndTime: time.Now(), Resource: resource.Empty()}.Snapshot()
	processor.OnEnd(large)
	_, span := provider.Tracer("synthetic").Start(t.Context(), "small")
	span.End()
	if err := provider.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := p.shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || p.dropped.Load() != 1 {
		t.Fatal(calls, p.dropped.Load())
	}
}

func TestOneCollectorIntegerRange(t *testing.T) {
	for _, value := range []float64{math.NaN(), math.Inf(1), 0x1p63, math.Nextafter(-0x1p63, math.Inf(-1))} {
		if _, ok := collectorInteger(value); ok {
			t.Fatal("invalid integer accepted")
		}
	}
	if value, ok := collectorInteger(-1.9); !ok || value != -1 {
		t.Fatal(value, ok)
	}
}

func TestOneCollectorComplexAttributeUsesEffectiveScrubber(t *testing.T) {
	registry := journal.NewRegistryScrubber()
	registry.Register([]byte(`synthetic-"quoted"-secret`))
	facts := onecollector.Facts{}
	addCollectorAttrs(facts, "attributes", []attribute.KeyValue{attribute.StringSlice("list", []string{`synthetic-"quoted"-secret`})}, registry)
	var wrapper string
	if err := json.Unmarshal(facts["attributes.list"], &wrapper); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(wrapper, "synthetic-") {
		t.Fatal("configured scrubber did not reach complex attributes")
	}
}
