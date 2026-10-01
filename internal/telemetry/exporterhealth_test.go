package telemetry

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

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

type flakyMetricExporter struct {
	err error
}

func (e *flakyMetricExporter) Temporality(kind metric.InstrumentKind) metricdata.Temporality {
	return metric.DefaultTemporalitySelector(kind)
}

func (e *flakyMetricExporter) Aggregation(kind metric.InstrumentKind) metric.Aggregation {
	return metric.DefaultAggregationSelector(kind)
}

func (e *flakyMetricExporter) Export(context.Context, *metricdata.ResourceMetrics) error {
	return e.err
}

func (e *flakyMetricExporter) ForceFlush(context.Context) error {
	return e.err
}

func (e *flakyMetricExporter) Shutdown(context.Context) error {
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
