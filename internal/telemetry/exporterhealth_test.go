package telemetry

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/goobers/goobers/internal/journal"
	telemetrytest "github.com/goobers/goobers/test/testsupport/telemetry"
)

func TestExporterHealthBoundsFailureDetailsAndRecovers(t *testing.T) {
	health := NewExporterHealth(true, string(ExporterOTLP), "https://user:secret@collector.example.com:4317/v1/traces?api_key=secret")
	privateErr := errors.New("dial C:\\private\\collector-token.txt: permission denied")

	health.RecordTraceFailure(privateErr)
	health.RecordTraceFailure(privateErr)
	failed := health.Snapshot()
	if !failed.Enabled || failed.Mode != string(ExporterOTLP) || failed.EndpointHost != "collector.example.com" || failed.EndpointClass != "dns-name" {
		t.Fatalf("endpoint snapshot leaked or misclassified details: %+v", failed)
	}
	if failed.Trace.State != "unhealthy" || failed.Trace.LastFailureReason != "exporter_error" ||
		failed.Trace.ConsecutiveFailures != 2 || failed.Trace.FailureTransitions != 1 ||
		failed.Trace.SuppressedFailureEvents != 1 {
		t.Fatalf("failure state = %+v", failed.Trace)
	}

	health.RecordTraceSuccess()
	recovered := health.Snapshot()
	if recovered.Trace.State != "healthy" || recovered.Trace.ConsecutiveFailures != 0 ||
		recovered.Trace.RecoveryTransitions != 1 || recovered.Trace.LastSuccessAt == nil {
		t.Fatalf("recovery state = %+v", recovered.Trace)
	}
}

func TestExporterHealthJournalsRateLimitedTransitions(t *testing.T) {
	schedulerDir := filepath.Join(t.TempDir(), "scheduler")
	log, _, err := journal.OpenInstanceLog(schedulerDir)
	if err != nil {
		t.Fatal(err)
	}
	privateErr := errors.New("dial C:\\private\\collector-token.txt: permission denied")
	health := NewExporterHealth(true, string(ExporterOTLP), "collector.example.com/v1/traces?api_key=secret")

	health.RecordTraceFailure(privateErr)
	health.AttachInstanceLog(log)
	health.RecordTraceFailure(privateErr)
	health.RecordTraceSuccess()
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	events, err := journal.ReadInstanceLog(schedulerDir)
	if err != nil {
		t.Fatal(err)
	}
	var transitions []journal.Event
	for _, event := range events {
		if event.Type == journal.EventRunnerAnnotation && event.Runner["annotation"] == exporterHealthTransitionAnnotation {
			transitions = append(transitions, event)
		}
	}
	if len(transitions) != 2 {
		t.Fatalf("transition events = %+v, want first failure and recovery only", transitions)
	}
	if transitions[0].Runner["state"] != exporterHealthStateUnhealthy || transitions[0].Runner["signal"] != "trace" ||
		transitions[0].Runner["reason"] != "exporter_error" || transitions[0].Runner["endpointHost"] != "collector.example.com" {
		t.Fatalf("failure transition = %+v", transitions[0])
	}
	if transitions[1].Runner["state"] != exporterHealthStateRecovered || transitions[1].Runner["signal"] != "trace" {
		t.Fatalf("recovery transition = %+v", transitions[1])
	}
	journalText := fmt.Sprint(events)
	for _, leaked := range []string{"collector-token.txt", "api_key", "v1/traces", "permission denied"} {
		if strings.Contains(journalText, leaked) {
			t.Fatalf("journal transition leaked %q in events: %+v", leaked, events)
		}
	}
}

// A persistently failing export writes telemetry_export_refused: on the first
// failure, then once per suppression window with the repeats it stands for,
// and again immediately after a recovery (#6417).
func TestExporterHealthJournalsExportRefusedFirstRepeatAndRecovery(t *testing.T) {
	schedulerDir := filepath.Join(t.TempDir(), "scheduler")
	log, _, err := journal.OpenInstanceLog(schedulerDir)
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	health := NewExporterHealth(true, string(ExporterOTLP), "https://user:secret@collector.example.com:4317/v1/traces?api_key=secret")
	health.now = func() time.Time { return clock }
	health.refusedWindow = 30 * time.Minute
	privateErr := errors.New("dial C:\\private\\collector-token.txt: permission denied")

	// Before the instance log exists: held, then written on attach.
	health.recordTraceExporterFailure(exporterHealthExporterOTLP, privateErr)
	health.AttachInstanceLog(log)
	for range 3 { // Suppressed repeats inside the window.
		clock = clock.Add(time.Minute)
		health.recordTraceExporterFailure(exporterHealthExporterOTLP, privateErr)
	}
	clock = clock.Add(30 * time.Minute) // The next repeat past the window.
	health.recordTraceExporterFailure(exporterHealthExporterOTLP, privateErr)
	health.recordTraceExporterSuccess(exporterHealthExporterOTLP)
	clock = clock.Add(time.Minute) // A new failing period is a first occurrence.
	health.recordTraceExporterFailure(exporterHealthExporterAzureMonitor, context.DeadlineExceeded)
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	events, err := journal.ReadInstanceLog(schedulerDir)
	if err != nil {
		t.Fatal(err)
	}
	var refused []journal.Event
	for _, event := range events {
		if event.Type == journal.EventError && event.Error != nil && event.Error.Code == exporterHealthRefusedCode {
			refused = append(refused, event)
		}
	}
	if len(refused) != 3 {
		t.Fatalf("refused events = %+v, want first, one windowed repeat, and post-recovery first", refused)
	}
	first, repeat, again := refused[0], refused[1], refused[2]
	if first.Runner["signal"] != "trace" || first.Runner["destination"] != exporterHealthExporterOTLP ||
		first.Runner["errorClass"] != "exporter_error" || first.Runner["endpointClass"] != "dns-name" ||
		first.Error.Message != "exporter_error" || first.Runner["repeats"] != nil {
		t.Fatalf("first refused event = %+v", first)
	}
	if repeat.Runner["repeats"] != float64(4) || repeat.Runner["window"] != "30m0s" {
		t.Fatalf("windowed repeat = %+v", repeat.Runner)
	}
	if again.Runner["destination"] != exporterHealthExporterAzureMonitor || again.Runner["errorClass"] != "deadline_exceeded" || again.Runner["repeats"] != nil {
		t.Fatalf("post-recovery refused event = %+v", again.Runner)
	}
	journalText := fmt.Sprint(events)
	for _, leaked := range []string{"collector-token.txt", "api_key", "secret", "v1/traces", "permission denied"} {
		if strings.Contains(journalText, leaked) {
			t.Fatalf("refused event leaked %q: %+v", leaked, refused)
		}
	}
}

func TestExporterHealthAggregatesDualOTLPAzureTraceExporters(t *testing.T) {
	health := NewExporterHealth(true, "custom", "collector.example.com:4317")
	health.ConfigureTrace()
	otlp := observedSpanExporter{
		next:     failingSpanExporter{err: context.DeadlineExceeded},
		health:   health,
		exporter: exporterHealthExporterOTLP,
	}
	azure := observedSpanExporter{
		next:     failingSpanExporter{},
		health:   health,
		exporter: exporterHealthExporterAzureMonitor,
	}

	if err := otlp.ExportSpans(context.Background(), nil); err == nil {
		t.Fatal("OTLP export unexpectedly succeeded")
	}
	if err := azure.ExportSpans(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	stillDegraded := health.Snapshot().Trace
	if stillDegraded.State != "unhealthy" || stillDegraded.LastFailureReason != "deadline_exceeded" ||
		stillDegraded.RecoveryTransitions != 0 || stillDegraded.ConsecutiveFailures != 1 {
		t.Fatalf("azure success masked otlp failure: %+v", stillDegraded)
	}

	otlpRecovered := observedSpanExporter{
		next:     failingSpanExporter{},
		health:   health,
		exporter: exporterHealthExporterOTLP,
	}
	if err := otlpRecovered.ExportSpans(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	recovered := health.Snapshot().Trace
	if recovered.State != "healthy" || recovered.RecoveryTransitions != 1 || recovered.ConsecutiveFailures != 0 {
		t.Fatalf("otlp recovery did not clear aggregate health: %+v", recovered)
	}
}

func TestClassifyEndpointDropsSchemeLessPathAndQuery(t *testing.T) {
	for _, tc := range []struct {
		endpoint  string
		wantHost  string
		wantClass string
	}{
		{endpoint: "collector.example.com/v1/traces?api_key=secret", wantHost: "collector.example.com", wantClass: "dns-name"},
		{endpoint: "collector.example.com:4317/v1/traces?api_key=secret", wantHost: "collector.example.com", wantClass: "dns-name"},
		{endpoint: "collector.example.com?api_key=secret", wantHost: "collector.example.com", wantClass: "dns-name"},
		{endpoint: "/v1/traces?api_key=secret", wantHost: "", wantClass: "unknown"},
	} {
		host, class := classifyEndpoint(tc.endpoint)
		if host != tc.wantHost || class != tc.wantClass {
			t.Fatalf("classifyEndpoint(%q) = (%q, %q), want (%q, %q)", tc.endpoint, host, class, tc.wantHost, tc.wantClass)
		}
		if strings.ContainsAny(host, "/?#") || strings.Contains(host, "secret") {
			t.Fatalf("classifyEndpoint(%q) leaked unsafe host %q", tc.endpoint, host)
		}
	}
}

func TestExporterHealthPreservesBoundedModes(t *testing.T) {
	for _, mode := range []string{"disabled", "local", "azure-monitor", string(ExporterOTLP), string(ExporterStdout)} {
		if got := NewExporterHealth(mode != "disabled", mode, "").Snapshot().Mode; got != mode {
			t.Fatalf("mode %q normalized to %q", mode, got)
		}
	}
	if got := NewExporterHealth(true, "private-new-mode", "").Snapshot().Mode; got != "custom" {
		t.Fatalf("unknown mode normalized to %q, want custom", got)
	}
}

func TestObservedMetricExporterRecordsMetricOnlyFailureAndRecovery(t *testing.T) {
	health := NewExporterHealth(true, string(ExporterOTLP), "127.0.0.1:4317")
	exporter := observedMetricExporter{next: &flakyMetricExporter{err: errors.New("metric collector unavailable")}, health: health}

	if err := exporter.Export(context.Background(), &metricdata.ResourceMetrics{}); err == nil {
		t.Fatal("Export unexpectedly succeeded")
	}
	failed := health.Snapshot()
	if failed.Trace.Configured {
		t.Fatalf("metric-only failure configured trace state: %+v", failed.Trace)
	}
	if failed.Metric.State != "unhealthy" || failed.Metric.LastFailureReason != "exporter_error" {
		t.Fatalf("metric failure state = %+v", failed.Metric)
	}

	exporter.next = &flakyMetricExporter{}
	if err := exporter.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	recovered := health.Snapshot()
	if recovered.Metric.State != "healthy" || recovered.Metric.RecoveryTransitions != 1 {
		t.Fatalf("metric recovery state = %+v", recovered.Metric)
	}
}

func TestDisabledExporterHealthSnapshotIsExplicit(t *testing.T) {
	got := DisabledExporterHealthSnapshot()
	if got.Enabled || got.Mode != "disabled" || got.Trace.State != "disabled" || got.Metric.State != "disabled" {
		t.Fatalf("disabled snapshot = %+v", got)
	}
}

func TestClientShutdownSuccessfulProviderClearsExporterHealth(t *testing.T) {
	health := NewExporterHealth(true, string(ExporterOTLP), "collector.example.com:4317")
	health.ConfigureTrace()
	health.RecordTraceFailure(errors.New("connection refused"))

	client, err := New(context.Background(), Config{
		SpanExporter:   telemetrytest.NewMemoryExporter(),
		ExporterHealth: health,
	})
	if err != nil {
		t.Fatal(err)
	}
	if before := health.Snapshot().Trace; before.State != "unhealthy" || before.LastSuccessAt != nil {
		t.Fatalf("pre-shutdown health = %+v", before)
	}
	if err := client.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	after := health.Snapshot().Trace
	if after.State != "healthy" || after.LastSuccessAt == nil || after.RecoveryTransitions != 1 || after.ConsecutiveFailures != 0 {
		t.Fatalf("post-shutdown health = %+v", after)
	}
}

func TestClientFlushDoesNotClearStartupDegradeWithoutRemoteExporter(t *testing.T) {
	health := NewExporterHealth(true, string(ExporterOTLP), "127.0.0.1:4317")
	health.RecordTraceFailure(errors.New("load CA file"))
	client, err := New(context.Background(), Config{
		SpanExporter:   telemetrytest.NewMemoryExporter(),
		ExporterHealth: health,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })

	if err := client.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := health.Snapshot().Trace
	if got.State != "unhealthy" || got.ConsecutiveFailures != 1 || got.RecoveryTransitions != 0 || got.LastSuccessAt != nil {
		t.Fatalf("local-only flush cleared startup degrade: %+v", got)
	}
}

func TestClientFlushProviderSuccessRecoversInstalledExporterFailures(t *testing.T) {
	health := NewExporterHealth(true, "custom", "collector.example.com:4317")
	health.configureTraceExporter(exporterHealthExporterOTLP)
	health.configureTraceExporter(exporterHealthExporterAzureMonitor)
	health.recordTraceExporterFailure(exporterHealthExporterOTLP, context.DeadlineExceeded)
	health.recordTraceExporterFailure(exporterHealthExporterAzureMonitor, errors.New("azure monitor unavailable"))
	client := &Client{tracerProvider: sdktrace.NewTracerProvider(), exporterHealth: health}

	if err := client.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	got := health.Snapshot().Trace
	if got.State != "healthy" || got.RecoveryTransitions != 1 || got.ConsecutiveFailures != 0 || got.LastSuccessAt == nil {
		t.Fatalf("provider ForceFlush did not recover installed exporter failures: %+v", got)
	}
}

func TestClientFlushProviderSuccessPreservesUncoveredSiblingFailure(t *testing.T) {
	health := NewExporterHealth(true, "custom", "collector.example.com:4317")
	health.configureTraceExporter(exporterHealthExporterOTLP)
	health.recordTraceExporterFailure(exporterHealthExporterOTLP, context.DeadlineExceeded)
	health.recordTraceExporterFailure(exporterHealthExporterAzureMonitor, errors.New("azure monitor unavailable"))
	client := &Client{tracerProvider: sdktrace.NewTracerProvider(), exporterHealth: health}

	if err := client.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	got := health.Snapshot().Trace
	if got.State != "unhealthy" || got.RecoveryTransitions != 0 || got.ConsecutiveFailures != 2 {
		t.Fatalf("provider ForceFlush masked uncovered sibling failure: %+v", got)
	}
}

func TestClientFlushFailedTraceExportIncrementsOnce(t *testing.T) {
	health := NewExporterHealth(true, string(ExporterOTLP), "127.0.0.1:4317")
	health.ConfigureTrace()
	exporter := observedSpanExporter{next: failingSpanExporter{err: context.DeadlineExceeded}, health: health}
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sdktrace.NewBatchSpanProcessor(exporter)))
	client := &Client{tracerProvider: provider, exporterHealth: health}
	_, span := provider.Tracer(ScopeName).Start(context.Background(), "flush-regression")
	span.End()

	if err := client.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := health.Snapshot().Trace
	if got.ConsecutiveFailures != 1 || got.FailureTransitions != 1 || got.SuppressedFailureEvents != 0 {
		t.Fatalf("one failed flush should increment once, got %+v", got)
	}
}

func TestClientShutdownFailedTraceExportMarksUnhealthy(t *testing.T) {
	health := NewExporterHealth(true, string(ExporterOTLP), "127.0.0.1:4317")
	health.configureTraceExporter(exporterHealthExporterOTLP)
	exporter := observedSpanExporter{
		next:     shutdownFailingSpanExporter{err: context.DeadlineExceeded},
		health:   health,
		exporter: exporterHealthExporterOTLP,
	}
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(exporter)))
	client := &Client{tracerProvider: provider, exporterHealth: health}

	if err := client.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := health.Snapshot().Trace
	if got.State != "unhealthy" || got.LastFailureReason != "deadline_exceeded" ||
		got.ConsecutiveFailures != 1 || got.FailureTransitions != 1 {
		t.Fatalf("failed trace shutdown health = %+v", got)
	}
}

func TestClientFlushFailedMetricExportMarksUnhealthyAndRecovers(t *testing.T) {
	health := NewExporterHealth(true, string(ExporterOTLP), "127.0.0.1:4317")
	exporter := &flakyMetricExporter{err: context.DeadlineExceeded}
	client := &Client{
		meterProvider: metric.NewMeterProvider(metric.WithReader(metric.NewPeriodicReader(
			observedMetricExporter{next: exporter, health: health, exporter: exporterHealthExporterOTLP},
		))),
		exporterHealth: health,
	}
	health.configureMetricExporter(exporterHealthExporterOTLP)

	if err := client.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	failed := health.Snapshot().Metric
	if failed.State != "unhealthy" || failed.LastFailureReason != "deadline_exceeded" ||
		failed.ConsecutiveFailures != 1 || failed.FailureTransitions != 1 {
		t.Fatalf("failed metric flush health = %+v", failed)
	}

	exporter.err = nil
	if err := client.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	recovered := health.Snapshot().Metric
	if recovered.State != "healthy" || recovered.RecoveryTransitions != 1 ||
		recovered.ConsecutiveFailures != 0 || recovered.LastSuccessAt == nil {
		t.Fatalf("recovered metric flush health = %+v", recovered)
	}
}

func TestClientShutdownFailedMetricExportMarksUnhealthy(t *testing.T) {
	health := NewExporterHealth(true, string(ExporterOTLP), "127.0.0.1:4317")
	exporter := &flakyMetricExporter{shutdownErr: context.DeadlineExceeded}
	client := &Client{
		meterProvider: metric.NewMeterProvider(metric.WithReader(metric.NewPeriodicReader(
			observedMetricExporter{next: exporter, health: health, exporter: exporterHealthExporterOTLP},
		))),
		exporterHealth: health,
	}
	health.configureMetricExporter(exporterHealthExporterOTLP)

	if err := client.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := health.Snapshot().Metric
	if got.State != "unhealthy" || got.LastFailureReason != "deadline_exceeded" ||
		got.ConsecutiveFailures != 1 || got.FailureTransitions != 1 {
		t.Fatalf("failed metric shutdown health = %+v", got)
	}
}

type flakyMetricExporter struct {
	err         error
	exportErr   error
	flushErr    error
	shutdownErr error
}

func (e *flakyMetricExporter) Temporality(kind metric.InstrumentKind) metricdata.Temporality {
	return metric.DefaultTemporalitySelector(kind)
}

func (e *flakyMetricExporter) Aggregation(kind metric.InstrumentKind) metric.Aggregation {
	return metric.DefaultAggregationSelector(kind)
}

func (e *flakyMetricExporter) Export(context.Context, *metricdata.ResourceMetrics) error {
	if e.exportErr != nil {
		return e.exportErr
	}
	return e.err
}

func (e *flakyMetricExporter) ForceFlush(context.Context) error {
	if e.flushErr != nil {
		return e.flushErr
	}
	return e.err
}

func (e *flakyMetricExporter) Shutdown(context.Context) error {
	if e.shutdownErr != nil {
		return e.shutdownErr
	}
	return e.err
}

type failingSpanExporter struct {
	err error
}

func (e failingSpanExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error {
	return e.err
}

func (e failingSpanExporter) Shutdown(context.Context) error {
	return nil
}

type shutdownFailingSpanExporter struct {
	err error
}

func (e shutdownFailingSpanExporter) ExportSpans(context.Context, []sdktrace.ReadOnlySpan) error {
	return nil
}

func (e shutdownFailingSpanExporter) Shutdown(context.Context) error {
	return e.err
}
