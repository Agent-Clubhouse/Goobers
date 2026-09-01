package telemetry

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	collectormetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/worktree"
	telemetrytest "github.com/goobers/goobers/test/testsupport/telemetry"
)

const testRunID = "0af7651916cd43dd8448eb211c80319c"

func TestOTLPExporterPushesMetricsToConfiguredCollector(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	collector := &recordingOTLPMetricCollector{
		requests: make(chan *collectormetrics.ExportMetricsServiceRequest, 4),
		headers:  make(chan metadata.MD, 4),
	}
	collectormetrics.RegisterMetricsServiceServer(server, collector)
	go func() {
		_ = server.Serve(listener)
	}()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})

	client, err := New(context.Background(), Config{
		ServiceName:          "telemetry-test",
		SpanExporter:         telemetrytest.NewMemoryExporter(),
		Exporter:             ExporterOTLP,
		OTLPEndpoint:         "http://" + listener.Addr().String(),
		OTLPInsecure:         true,
		OTLPHeaders:          map[string]string{"authorization": "******"},
		MetricExportInterval: 50 * time.Millisecond,
		Batch:                true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })

	_, span, err := client.StartRun(context.Background(), RunAttributes{
		Gaggle: "acme-web", WorkflowID: "implement", RunID: testRunID,
	})
	if err != nil {
		t.Fatal(err)
	}
	span.Complete(OutcomeSuccess, false)

	deadline := time.After(10 * time.Second)
	for {
		select {
		case req := <-collector.requests:
			if metricNamesIn(req).has(MetricRunDuration) {
				select {
				case headers := <-collector.headers:
					if got := headers.Get("authorization"); len(got) != 1 || got[0] != "******" {
						t.Fatalf("authorization metadata = %q, want configured collector credential", got)
					}
				default:
					t.Fatal("collector received no OTLP metadata")
				}
				return
			}
		case <-deadline:
			t.Fatal("collector did not receive the run duration metric")
		}
	}
}

func TestPeriodicMetricReaderExportsWithoutFlush(t *testing.T) {
	exporter := &countingMetricExporter{}
	client := newMetricsClient(t, Config{MetricExporter: exporter, MetricExportInterval: 20 * time.Millisecond})

	_, span, err := client.StartRun(context.Background(), RunAttributes{
		Gaggle: "acme-web", WorkflowID: "implement", RunID: testRunID,
	})
	if err != nil {
		t.Fatal(err)
	}
	span.Complete(OutcomeSuccess, false)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if exporter.names().has(MetricRunDuration) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("periodic reader did not export %s", MetricRunDuration)
}

func TestForceFlushExportsPendingMetrics(t *testing.T) {
	exporter := &countingMetricExporter{}
	client := newMetricsClient(t, Config{MetricExporter: exporter, MetricExportInterval: time.Hour})

	_, span, err := client.StartRun(context.Background(), RunAttributes{
		Gaggle: "acme-web", WorkflowID: "implement", RunID: testRunID,
	})
	if err != nil {
		t.Fatal(err)
	}
	span.Complete(OutcomeSuccess, false)

	if err := client.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !exporter.names().has(MetricRunDuration) {
		t.Fatalf("force flush exported %v, want %s", exporter.names(), MetricRunDuration)
	}
	if err := client.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := exporter.shutdowns.Load(); got != 1 {
		t.Fatalf("metric exporter shutdowns = %d, want 1", got)
	}
}

func TestRunStageGateAndRetryMetricsAreRecorded(t *testing.T) {
	reader := metric.NewManualReader()
	client := newMetricsClient(t, Config{MetricReader: reader})
	ctx := context.Background()

	runCtx, runSpan, err := client.StartRun(ctx, RunAttributes{
		Gaggle: "acme-web", WorkflowID: "implement", RunID: testRunID,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, taskSpan, err := client.StartTask(runCtx, TaskAttributes{
		Gaggle: "acme-web", WorkflowID: "implement", RunID: testRunID,
		TaskID: "build", TaskType: StageTypeDeterministic, Attempt: 2, AttemptKind: AttemptKindInfra,
	})
	if err != nil {
		t.Fatal(err)
	}
	taskSpan.CompleteWithError("failure", "BUILD_FAILED", true)

	_, gateSpan, err := client.StartGate(runCtx, GateAttributes{
		Gaggle: "acme-web", WorkflowID: "implement", RunID: testRunID, GateID: "review",
	})
	if err != nil {
		t.Fatal(err)
	}
	gateSpan.SetGateResult("escalated", 2)
	gateSpan.Complete("escalated", false)
	runSpan.Complete(OutcomeSuccess, false)

	collected := collectMetrics(t, reader)
	runPoints := metricPoints(t, collected, MetricRunDuration)
	if len(runPoints) != 1 || runPoints[0].count != 1 {
		t.Fatalf("%s points = %+v, want one histogram point", MetricRunDuration, runPoints)
	}
	assertPointAttr(t, runPoints[0], AttrWorkflow, "implement")
	assertPointAttr(t, runPoints[0], AttrOutcome, OutcomeSuccess)

	stagePoints := metricPoints(t, collected, MetricStageOutcomes)
	if len(stagePoints) != 2 {
		t.Fatalf("%s points = %d, want one per finished stage", MetricStageOutcomes, len(stagePoints))
	}
	failed := pointWith(t, stagePoints, AttrStage, "build")
	assertPointAttr(t, failed, AttrErrorCode, "BUILD_FAILED")
	assertPointAttr(t, failed, AttrStageType, StageTypeDeterministic)

	retries := metricPoints(t, collected, MetricStageRetries)
	if len(retries) != 1 || retries[0].value != 1 {
		t.Fatalf("%s points = %+v, want a single retry", MetricStageRetries, retries)
	}
	assertPointAttr(t, retries[0], AttrAttemptKind, AttemptKindInfra)

	decisions := metricPoints(t, collected, MetricGateDecisions)
	if len(decisions) != 1 || decisions[0].value != 1 {
		t.Fatalf("%s points = %+v, want a single decision", MetricGateDecisions, decisions)
	}
	assertPointAttr(t, decisions[0], AttrGateDecision, "escalated")

	escalations := metricPoints(t, collected, MetricEscalations)
	if len(escalations) != 1 || escalations[0].value != 2 {
		t.Fatalf("%s points = %+v, want the gate decision and its outcome", MetricEscalations, escalations)
	}

	for _, point := range metricPoints(t, collected, MetricWorkActive) {
		if point.value != 0 {
			t.Fatalf("%s = %v for %v, want every finished span decremented", MetricWorkActive, point.value, point.attrs)
		}
	}
}

func TestMetricAttributesExcludeHighCardinalityAndSensitiveValues(t *testing.T) {
	reader := metric.NewManualReader()
	client := newMetricsClient(t, Config{MetricReader: reader})
	ctx := context.Background()

	runCtx, runSpan, err := client.StartRun(ctx, RunAttributes{
		Gaggle: "acme-web", WorkflowID: "implement", RunID: testRunID,
		WorkflowDigest: "sha256:deadbeef", GooberDigest: "sha256:feedface",
		ItemID: "4073", ItemURL: "https://github.com/Agent-Clubhouse/Goobers/issues/4073",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, taskSpan, err := client.StartTask(runCtx, TaskAttributes{
		Gaggle: "acme-web", WorkflowID: "implement", RunID: testRunID,
		TaskID: "implement", TaskType: StageTypeAgentic, Model: "gpt-5", HarnessVersion: "1.2.3",
		Branch: 3, ItemID: "4073", ItemURL: "https://github.com/Agent-Clubhouse/Goobers/issues/4073",
	})
	if err != nil {
		t.Fatal(err)
	}
	taskSpan.Complete(OutcomeSuccess, false)
	runSpan.Complete(OutcomeSuccess, false)

	forbidden := []string{
		AttrRunID, AttrGaggle, AttrItemID, AttrItemURL, AttrWorktreeID,
		AttrWorkflowDigest, AttrGooberDigest, AttrBranch, AttrHarnessVersion,
		AttrErrorMessage, AttrAgentID,
	}
	collected := collectMetrics(t, reader)
	for _, scope := range collected.ScopeMetrics {
		for _, m := range scope.Metrics {
			for _, point := range dataPoints(t, m) {
				for _, key := range forbidden {
					if _, found := point.attrs.Value(attribute.Key(key)); found {
						t.Errorf("metric %s carries forbidden attribute %s", m.Name, key)
					}
				}
			}
		}
	}
	if points := metricPoints(t, collected, MetricStageDuration); len(points) == 1 {
		assertPointAttr(t, points[0], AttrModel, "gpt-5")
	} else {
		t.Fatalf("%s points = %d, want 1", MetricStageDuration, len(points))
	}
}

func TestMetricAttributeCardinalityIsBounded(t *testing.T) {
	limiter := newCardinalityLimiter(2)
	for _, testCase := range []struct {
		value string
		want  string
	}{
		{value: "alpha", want: "alpha"},
		{value: "beta", want: "beta"},
		{value: "alpha", want: "alpha"},
		{value: "gamma", want: metricAttributeOverflow},
		{value: "https://example.invalid/issues/1", want: metricAttributeOverflow},
		{value: strings.Repeat("a", metricAttributeMaxLength+1), want: metricAttributeOverflow},
	} {
		got, ok := limiter.bound(AttrStage, testCase.value)
		if !ok || got != testCase.want {
			t.Errorf("bound(%q) = %q (%t), want %q", testCase.value, got, ok, testCase.want)
		}
	}
	if _, ok := limiter.bound(AttrStage, "  "); ok {
		t.Error("bound accepted a blank value")
	}
}

func TestUnboundedStageNamesCollapseInMetrics(t *testing.T) {
	reader := metric.NewManualReader()
	client := newMetricsClient(t, Config{MetricReader: reader})
	client.instruments.limiter = newCardinalityLimiter(1)

	for _, stage := range []string{"first", "second", "third"} {
		_, span, err := client.StartTask(context.Background(), TaskAttributes{
			Gaggle: "acme-web", WorkflowID: "implement", RunID: testRunID,
			TaskID: stage, TaskType: StageTypeDeterministic,
		})
		if err != nil {
			t.Fatal(err)
		}
		span.Complete(OutcomeSuccess, false)
	}

	stages := make(map[string]struct{})
	for _, point := range metricPoints(t, collectMetrics(t, reader), MetricStageOutcomes) {
		value, _ := point.attrs.Value(attribute.Key(AttrStage))
		stages[value.AsString()] = struct{}{}
	}
	if _, overflowed := stages[metricAttributeOverflow]; !overflowed || len(stages) != 2 {
		t.Fatalf("%s stage values = %v, want the bound plus %q", MetricStageOutcomes, stages, metricAttributeOverflow)
	}
}

func TestStageEmittedMetricsAreExportedAsMetrics(t *testing.T) {
	reader := metric.NewManualReader()
	client := newMetricsClient(t, Config{MetricReader: reader})

	workspace := t.TempDir()
	dir := PrepareStageTelemetryDir(workspace)
	if dir == "" {
		t.Fatal("stage telemetry directory was not prepared")
	}
	records := `{"name":"files.changed","value":4,"unit":"{file}"}` + "\n" +
		`{"name":"lines.added","value":120}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, metricsFile), []byte(records), 0o600); err != nil {
		t.Fatal(err)
	}

	_, span, err := client.StartTask(context.Background(), TaskAttributes{
		Gaggle: "acme-web", WorkflowID: "implement", RunID: testRunID,
		TaskID: "implement", TaskType: StageTypeAgentic,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := apiv1.ResultEnvelope{}
	IngestStageEmissions(dir, &result, span)
	span.Complete(OutcomeSuccess, false)

	points := metricPoints(t, collectMetrics(t, reader), MetricStageMetricValue)
	if len(points) != 2 {
		t.Fatalf("%s points = %d, want one per emitted metric", MetricStageMetricValue, len(points))
	}
	changed := pointWith(t, points, metricNameAttribute, "files.changed")
	if changed.value != 4 {
		t.Fatalf("files.changed = %v, want 4", changed.value)
	}
	assertPointAttr(t, changed, metricUnitAttribute, "{file}")
	assertPointAttr(t, changed, AttrStage, "implement")
	if result.Metrics["files.changed"] != 4 {
		t.Fatalf("result metrics = %v, want the span-event merge preserved", result.Metrics)
	}
}

func TestWorkcopyUsageIsExportedAsGauges(t *testing.T) {
	reader := metric.NewManualReader()
	client := newMetricsClient(t, Config{MetricReader: reader})

	client.RecordWorkcopyUsage(context.Background(), worktree.UsageMeasurement{
		Gaggle:           "acme-web",
		Operation:        worktree.UsageOperationCreate,
		OwnerRunID:       testRunID,
		WorktreeID:       "wt-c91a3be152bfaea1b",
		WorktreeMeasured: true,
		WorktreeBytes:    2048,
		WorkcopyMeasured: true,
		WorkcopyBytes:    8192,
	})

	collected := collectMetrics(t, reader)
	worktreePoints := metricPoints(t, collected, EventWorktreeDiskUsage)
	if len(worktreePoints) != 1 || worktreePoints[0].value != 2048 {
		t.Fatalf("%s points = %+v, want 2048 bytes", EventWorktreeDiskUsage, worktreePoints)
	}
	assertPointAttr(t, worktreePoints[0], AttrStorageOperation, string(worktree.UsageOperationCreate))
	workcopyPoints := metricPoints(t, collected, EventWorkcopyDiskUsage)
	if len(workcopyPoints) != 1 || workcopyPoints[0].value != 8192 {
		t.Fatalf("%s points = %+v, want 8192 bytes", EventWorkcopyDiskUsage, workcopyPoints)
	}
}

func TestMetricExportToUnavailableCollectorDoesNotFailWork(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	client, err := New(context.Background(), Config{
		ServiceName:          "telemetry-test",
		SpanExporter:         telemetrytest.NewMemoryExporter(),
		Exporter:             ExporterOTLP,
		OTLPEndpoint:         endpoint,
		OTLPInsecure:         true,
		MetricExportInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, span, err := client.StartRun(context.Background(), RunAttributes{
		Gaggle: "acme-web", WorkflowID: "implement", RunID: testRunID,
	})
	if err != nil {
		t.Fatal(err)
	}
	span.Complete(OutcomeSuccess, false)

	flushCtx, cancelFlush := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelFlush()
	if err := client.Flush(flushCtx); err != nil {
		t.Fatalf("flush with an unavailable collector: %v", err)
	}
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelShutdown()
	if err := client.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown with an unavailable collector: %v", err)
	}
}

func TestMetricExporterRequiresExplicitEndpoint(t *testing.T) {
	if _, err := metricExporter(context.Background(), Config{Exporter: ExporterOTLP}); err == nil {
		t.Fatal("metric exporter accepted an unconfigured endpoint")
	}
}

func TestMetricExporterTLSFailureDegradesInsteadOfFailing(t *testing.T) {
	client, err := New(context.Background(), Config{
		ServiceName:  "telemetry-test",
		SpanExporter: telemetrytest.NewMemoryExporter(),
		Exporter:     ExporterOTLP,
		OTLPEndpoint: "127.0.0.1:4317",
		OTLPCAFile:   filepath.Join(t.TempDir(), "missing.pem"),
	})
	if !errors.Is(err, ErrOTLPUnavailable) {
		t.Fatalf("New error = %v, want ErrOTLPUnavailable", err)
	}
	if client == nil {
		t.Fatal("New returned no client for a degraded OTLP exporter")
	}
	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })
	if _, span, spanErr := client.StartRun(context.Background(), RunAttributes{
		Gaggle: "acme-web", WorkflowID: "implement", RunID: testRunID,
	}); spanErr != nil {
		t.Fatal(spanErr)
	} else {
		span.Complete(OutcomeSuccess, false)
	}
}

func TestLocalOnlyClientRecordsNoMetrics(t *testing.T) {
	client, err := New(context.Background(), Config{
		ServiceName:  "telemetry-test",
		SpanExporter: telemetrytest.NewMemoryExporter(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })

	if client.instruments != nil {
		t.Fatal("a client without a metric reader built instruments")
	}
	_, span, err := client.StartRun(context.Background(), RunAttributes{
		Gaggle: "acme-web", WorkflowID: "implement", RunID: testRunID,
	})
	if err != nil {
		t.Fatal(err)
	}
	span.Complete(OutcomeSuccess, false)
}

func newMetricsClient(t *testing.T, cfg Config) *Client {
	t.Helper()
	cfg.ServiceName = "telemetry-test"
	cfg.SpanExporter = telemetrytest.NewMemoryExporter()
	client, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })
	if client.instruments == nil {
		t.Fatal("client built no metric instruments")
	}
	return client
}

type collectedPoint struct {
	attrs attribute.Set
	value float64
	count uint64
}

func collectMetrics(t *testing.T, reader metric.Reader) metricdata.ResourceMetrics {
	t.Helper()
	var collected metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &collected); err != nil {
		t.Fatal(err)
	}
	return collected
}

func metricPoints(t *testing.T, collected metricdata.ResourceMetrics, name string) []collectedPoint {
	t.Helper()
	var points []collectedPoint
	for _, scope := range collected.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name == name {
				points = append(points, dataPoints(t, m)...)
			}
		}
	}
	return points
}

func dataPoints(t *testing.T, m metricdata.Metrics) []collectedPoint {
	t.Helper()
	var points []collectedPoint
	switch data := m.Data.(type) {
	case metricdata.Sum[int64]:
		for _, point := range data.DataPoints {
			points = append(points, collectedPoint{attrs: point.Attributes, value: float64(point.Value)})
		}
	case metricdata.Gauge[int64]:
		for _, point := range data.DataPoints {
			points = append(points, collectedPoint{attrs: point.Attributes, value: float64(point.Value)})
		}
	case metricdata.Histogram[float64]:
		for _, point := range data.DataPoints {
			points = append(points, collectedPoint{attrs: point.Attributes, value: point.Sum, count: point.Count})
		}
	default:
		t.Fatalf("metric %s has unexpected data type %T", m.Name, m.Data)
	}
	return points
}

func pointWith(t *testing.T, points []collectedPoint, key, want string) collectedPoint {
	t.Helper()
	for _, point := range points {
		if value, found := point.attrs.Value(attribute.Key(key)); found && value.AsString() == want {
			return point
		}
	}
	t.Fatalf("no point with %s=%q in %+v", key, want, points)
	return collectedPoint{}
}

func assertPointAttr(t *testing.T, point collectedPoint, key, want string) {
	t.Helper()
	value, found := point.attrs.Value(attribute.Key(key))
	if !found || value.AsString() != want {
		t.Errorf("point attribute %s = %q (%t), want %q", key, value.AsString(), found, want)
	}
}

type metricNames map[string]struct{}

func (n metricNames) has(name string) bool {
	_, found := n[name]
	return found
}

func metricNamesIn(req *collectormetrics.ExportMetricsServiceRequest) metricNames {
	names := make(metricNames)
	for _, resource := range req.GetResourceMetrics() {
		for _, scope := range resource.GetScopeMetrics() {
			for _, m := range scope.GetMetrics() {
				names[m.GetName()] = struct{}{}
			}
		}
	}
	return names
}

type recordingOTLPMetricCollector struct {
	collectormetrics.UnimplementedMetricsServiceServer
	requests chan *collectormetrics.ExportMetricsServiceRequest
	headers  chan metadata.MD
}

func (c *recordingOTLPMetricCollector) Export(
	ctx context.Context,
	req *collectormetrics.ExportMetricsServiceRequest,
) (*collectormetrics.ExportMetricsServiceResponse, error) {
	if headers, ok := metadata.FromIncomingContext(ctx); ok {
		select {
		case c.headers <- headers:
		default:
		}
	}
	select {
	case c.requests <- req:
	default:
	}
	return &collectormetrics.ExportMetricsServiceResponse{}, nil
}

type countingMetricExporter struct {
	mu        sync.Mutex
	exported  metricNames
	shutdowns atomic.Int64
}

func (e *countingMetricExporter) Temporality(kind metric.InstrumentKind) metricdata.Temporality {
	return metric.DefaultTemporalitySelector(kind)
}

func (e *countingMetricExporter) Aggregation(kind metric.InstrumentKind) metric.Aggregation {
	return metric.DefaultAggregationSelector(kind)
}

func (e *countingMetricExporter) Export(_ context.Context, collected *metricdata.ResourceMetrics) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.exported == nil {
		e.exported = make(metricNames)
	}
	for _, scope := range collected.ScopeMetrics {
		for _, m := range scope.Metrics {
			e.exported[m.Name] = struct{}{}
		}
	}
	return nil
}

func (e *countingMetricExporter) ForceFlush(context.Context) error { return nil }

func (e *countingMetricExporter) Shutdown(context.Context) error {
	e.shutdowns.Add(1)
	return nil
}

func (e *countingMetricExporter) names() metricNames {
	e.mu.Lock()
	defer e.mu.Unlock()
	names := make(metricNames, len(e.exported))
	for name := range e.exported {
		names[name] = struct{}{}
	}
	return names
}
