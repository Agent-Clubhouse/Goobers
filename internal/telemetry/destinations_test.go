package telemetry

import (
	"bytes"
	"context"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	collectorlogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collectormetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/goobers/goobers/internal/journal"
	telemetrytest "github.com/goobers/goobers/test/testsupport/telemetry"
)

type namedTraceReceiver struct {
	collectortrace.UnimplementedTraceServiceServer
	requests chan *collectortrace.ExportTraceServiceRequest
	reject   bool
}

func (r *namedTraceReceiver) Export(_ context.Context, req *collectortrace.ExportTraceServiceRequest) (*collectortrace.ExportTraceServiceResponse, error) {
	if r.reject {
		return nil, status.Error(codes.PermissionDenied, "fixture rejection")
	}
	r.requests <- req
	return &collectortrace.ExportTraceServiceResponse{}, nil
}

type namedCollector struct {
	endpoint string
	traces   *namedTraceReceiver
	logs     *journalLogsReceiver
	metrics  *recordingOTLPMetricCollector
}

func startNamedCollector(t *testing.T, reject bool, stall ...bool) namedCollector {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var options []grpc.ServerOption
	if len(stall) > 0 && stall[0] {
		options = append(options, grpc.UnaryInterceptor(func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}))
	}
	server := grpc.NewServer(options...)
	c := namedCollector{endpoint: listener.Addr().String(), traces: &namedTraceReceiver{requests: make(chan *collectortrace.ExportTraceServiceRequest, 64), reject: reject}, logs: &journalLogsReceiver{requests: make(chan *collectorlogs.ExportLogsServiceRequest, 64)}, metrics: &recordingOTLPMetricCollector{requests: make(chan *collectormetrics.ExportMetricsServiceRequest, 64)}}
	// These fixtures collect headers even when no assertion needs them.
	c.logs.headers = makeHeaderChannel()
	c.metrics.headers = makeHeaderChannel()
	collectortrace.RegisterTraceServiceServer(server, c.traces)
	collectorlogs.RegisterLogsServiceServer(server, c.logs)
	collectormetrics.RegisterMetricsServiceServer(server, c.metrics)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { server.Stop(); _ = listener.Close() })
	return c
}
func makeHeaderChannel() chan metadata.MD { return make(chan metadata.MD, 64) }
func namedOTLP(name, endpoint string) NamedDestination {
	return NamedDestination{Name: name, Config: Config{Exporter: ExporterOTLP, OTLPEndpoint: endpoint, OTLPInsecure: true, JournalLogs: true}}
}
func closeNamedClient(t *testing.T, c *Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = c.Shutdown(ctx)
}
func receiveNamed[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(3 * time.Second):
		t.Fatal("destination did not receive telemetry")
		var zero T
		return zero
	}
}
func emitNamedRun(t *testing.T, c *Client) {
	t.Helper()
	_, span, err := c.StartRun(context.Background(), RunAttributes{Gaggle: "fixture", WorkflowID: "fixture", RunID: testRunID})
	if err != nil {
		t.Fatal(err)
	}
	span.Complete(OutcomeSuccess, false)
}

func TestNamedDestinationsFanoutAllSignalsAndLocalOnce(t *testing.T) {
	first, second := startNamedCollector(t, false), startNamedCollector(t, false)
	local := telemetrytest.NewMemoryExporter()
	root := t.TempDir()
	health := NewExporterHealth(true, "custom", "")
	c, err := New(t.Context(), Config{SpanExporter: local, JournalRoot: root, JournalInstanceID: "persistent-instance", ExporterHealth: health, ResourceAttributes: []attribute.KeyValue{attribute.String("goobers.instance.id", "persistent-instance")}, Destinations: []NamedDestination{namedOTLP("first", first.endpoint), namedOTLP("second", second.endpoint)}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeNamedClient(t, c) })
	emitNamedRun(t, c)
	log, _, err := journal.OpenInstanceLog(filepath.Join(root, "scheduler"))
	if err != nil {
		t.Fatal(err)
	}
	log.AppendBestEffort(journal.Event{Type: journal.EventRunnerAnnotation, Runner: map[string]any{"fixture": "one-commit"}})
	_ = log.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := c.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	a, b := receiveNamed(t, first.traces.requests), receiveNamed(t, second.traces.requests)
	as, bs := a.ResourceSpans[0].ScopeSpans[0].Spans, b.ResourceSpans[0].ScopeSpans[0].Spans
	if len(as) != 1 || len(bs) != 1 || !bytes.Equal(as[0].TraceId, bs[0].TraceId) || !bytes.Equal(as[0].SpanId, bs[0].SpanId) {
		t.Fatal("fanout changed or duplicated span identity")
	}
	if got := len(local.Spans()); got != 1 {
		t.Fatalf("local spans = %d", got)
	}
	for _, collector := range []namedCollector{first, second} {
		logs := receiveNamed(t, collector.logs.requests)
		if len(logs.ResourceLogs) != 1 || len(logs.ResourceLogs[0].ScopeLogs[0].LogRecords) != 1 {
			t.Fatalf("duplicate journal export: %v", logs)
		}
		if !metricNamesIn(receiveNamed(t, collector.metrics.requests)).has(MetricRunDuration) {
			t.Fatal("missing metric")
		}
	}
	stats := c.DestinationJournalStats()
	if len(stats) != 2 || stats["first"].Accepted != 1 || stats["second"].Accepted != 1 {
		t.Fatalf("stats: %+v", stats)
	}
	snapshot := health.Snapshot()
	if snapshot.Destinations["first"].Trace.State != "healthy" || snapshot.Destinations["second"].Trace.State != "healthy" {
		t.Fatalf("health: %+v", snapshot)
	}
}

func TestNamedDestinationFailureCannotMaskHealthyOrLocal(t *testing.T) {
	bad, good := startNamedCollector(t, true), startNamedCollector(t, false)
	local := telemetrytest.NewMemoryExporter()
	health := NewExporterHealth(true, "custom", "")
	c, err := New(t.Context(), Config{SpanExporter: local, ExporterHealth: health, Destinations: []NamedDestination{namedOTLP("bad", bad.endpoint), namedOTLP("good", good.endpoint)}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeNamedClient(t, c) })
	emitNamedRun(t, c)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_ = c.Flush(ctx)
	receiveNamed(t, good.traces.requests)
	if len(local.Spans()) != 1 {
		t.Fatal("remote failure blocked local export")
	}
	s := health.Snapshot()
	if s.Destinations["bad"].Trace.State != "unhealthy" || s.Destinations["good"].Trace.State != "healthy" {
		t.Fatalf("cross-destination health masking: %+v", s)
	}
	closeNamedClient(t, c)
	if health.Snapshot().Destinations["bad"].Trace.State != "unhealthy" {
		t.Fatal("shutdown without delivery falsely recovered destination")
	}
}

func TestNamedInvalidTLSDegradesOnlyItsDestination(t *testing.T) {
	good := startNamedCollector(t, false)
	local := telemetrytest.NewMemoryExporter()
	health := NewExporterHealth(true, "custom", "")
	bad := namedOTLP("bad-tls", "127.0.0.1:4317")
	bad.Config.OTLPInsecure = false
	bad.Config.OTLPCAFile = filepath.Join(t.TempDir(), "absent.pem")
	c, err := New(t.Context(), Config{SpanExporter: local, ExporterHealth: health, Destinations: []NamedDestination{bad, namedOTLP("good", good.endpoint)}})
	if c == nil || !errors.Is(err, ErrOTLPUnavailable) {
		t.Fatalf("client %v, error %v", c, err)
	}
	t.Cleanup(func() { closeNamedClient(t, c) })
	emitNamedRun(t, c)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_ = c.Flush(ctx)
	receiveNamed(t, good.traces.requests)
	if len(local.Spans()) != 1 || health.Snapshot().Destinations["bad-tls"].Trace.State != "unhealthy" {
		t.Fatal("bad TLS lost local spans or health")
	}
}

func TestNamedDestinationValidationRejectsBeforeBuilding(t *testing.T) {
	for _, names := range [][]string{{"same", "same"}, {"../outside"}, {""}} {
		var destinations []NamedDestination
		for _, name := range names {
			destinations = append(destinations, namedOTLP(name, "127.0.0.1:1"))
		}
		c, err := New(t.Context(), Config{Destinations: destinations})
		if c != nil || err == nil || !strings.Contains(err.Error(), "names") {
			t.Fatalf("names %q client %v err %v", names, c, err)
		}
	}
}

func TestNamedStalledDestinationDoesNotConsumeOtherSignalsFlushBudget(t *testing.T) {
	stalled, good := startNamedCollector(t, false, true), startNamedCollector(t, false)
	c, err := New(t.Context(), Config{SpanExporter: telemetrytest.NewMemoryExporter(), Destinations: []NamedDestination{namedOTLP("stalled", stalled.endpoint), namedOTLP("good", good.endpoint)}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeNamedClient(t, c) })
	emitNamedRun(t, c)
	c.Commit(journal.CommittedEvent{Kind: "run", JournalID: testRunID, RunID: testRunID, Seq: 1, Time: time.Now(), Body: []byte(`{"type":"fixture"}`)})
	ctx, cancel := context.WithTimeout(t.Context(), 400*time.Millisecond)
	defer cancel()
	start := time.Now()
	_ = c.Flush(ctx)
	if time.Since(start) > 2*time.Second {
		t.Fatal("flush exceeded caller budget")
	}
	receiveNamed(t, good.traces.requests)
	receiveNamed(t, good.logs.requests)
	receiveNamed(t, good.metrics.requests)
}

func TestNamedDiagnosticDestinationsKeepIndependentQueuesAndPrivacy(t *testing.T) {
	stalled, good := startNamedCollector(t, false, true), startNamedCollector(t, false)
	registry := journal.NewRegistryScrubber()
	registry.Register([]byte("private-fixture-value"))
	d, err := NewDiagnosticExporter(Config{Scrubber: registry, Destinations: []NamedDestination{namedOTLP("stalled", stalled.endpoint), namedOTLP("good", good.endpoint)}})
	if err != nil {
		t.Fatal(err)
	}
	if !d.Emit(DiagnosticRecord{Name: "goobers.service.health", Time: time.Now(), Attributes: map[string]any{"fixture": "private-fixture-value"}}) {
		t.Fatal("diagnostic admission failed")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 400*time.Millisecond)
	defer cancel()
	_ = d.Shutdown(ctx)
	got := receiveNamed(t, good.logs.requests)
	if strings.Contains(got.String(), "private-fixture-value") {
		t.Fatal("diagnostic secret reached collector")
	}
	stats := d.DestinationStats()
	if len(stats) != 2 || stats["good"].Delivered != 1 || stats["stalled"].Delivered != 0 {
		t.Fatalf("stats: %+v", stats)
	}
}

func TestNamedDestinationsRejectAmbiguousTransportForEveryConstructor(t *testing.T) {
	for _, exporter := range []ExporterKind{"", ExporterOTLP} {
		cfg := Config{Destinations: []NamedDestination{{Name: "ambiguous", Config: Config{Exporter: exporter, OTLPEndpoint: "127.0.0.1:4317", AzureMonitorConnectionString: "InstrumentationKey=fixture;IngestionEndpoint=http://127.0.0.1:1"}}}}
		if client, err := New(t.Context(), cfg); client != nil || err == nil {
			t.Fatalf("client=%v err=%v; mixed transport accepted", client, err)
		}
		if client, err := NewDiagnosticExporter(cfg); client != nil || err == nil {
			t.Fatalf("diagnostic client=%v err=%v; mixed transport accepted", client, err)
		}
	}
}
