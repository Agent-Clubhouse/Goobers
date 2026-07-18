package telemetry

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestIngestStageEmissionsMergesMetricsAndAnnotatesSpan(t *testing.T) {
	dir := PrepareStageTelemetryDir(t.TempDir())
	if err := os.WriteFile(filepath.Join(dir, metricsFile), []byte(
		"{\"name\":\"exitCode\",\"value\":99}\n"+
			"{\"name\":\"build.items\",\"value\":42,\"unit\":\"count\",\"attrs\":{\"source\":\"scan\",\"cached\":true}}\n"+
			"not-json\n"+
			"{\"name\":\"\",\"value\":1}\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}
	const eventTime = "2026-07-18T18:00:00Z"
	if err := os.WriteFile(filepath.Join(dir, eventsFile), []byte(
		"{\"ts\":\""+eventTime+"\",\"name\":\"scan.complete\",\"attrs\":{\"files\":42}}\n"+
			"{\"ts\":\"not-a-time\",\"name\":\"bad\"}\n"+
			"\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}

	exporter := NewMemoryExporter()
	client, err := New(context.Background(), Config{ServiceName: "stage-emission-test", SpanExporter: exporter})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })
	runID, err := NewRunID()
	if err != nil {
		t.Fatal(err)
	}
	_, span, err := client.StartTask(context.Background(), TaskAttributes{
		Gaggle: "web", WorkflowID: "wf", WorkflowVersion: "1", RunID: runID, TaskID: "scan",
	})
	if err != nil {
		t.Fatal(err)
	}

	result := apiv1.ResultEnvelope{Metrics: map[string]float64{"exitCode": 0}}
	IngestStageEmissions(dir, &result, span)
	span.Succeed("done")

	if got := result.Metrics["exitCode"]; got != 0 {
		t.Fatalf("runner-computed exitCode = %v, want 0 (stage emission must lose)", got)
	}
	if got := result.Metrics["build.items"]; got != 42 {
		t.Fatalf("build.items = %v, want 42", got)
	}

	spans := exporter.Spans()
	if len(spans) != 1 {
		t.Fatalf("spans = %d, want 1", len(spans))
	}
	events := spans[0].Events()
	if len(events) != 5 {
		t.Fatalf("span events = %d, want 5: %#v", len(events), events)
	}
	assertSpanEventAttribute(t, events[1].Attributes, emissionKindAttribute, emissionKindMetric)
	assertSpanEventAttribute(t, events[1].Attributes, metricValueAttribute, "42")
	assertSpanEventAttribute(t, events[1].Attributes, metricUnitAttribute, "count")
	assertSpanEventAttribute(t, events[1].Attributes, "source", "scan")
	assertSpanEventAttribute(t, events[1].Attributes, "cached", "true")

	wantTime, err := time.Parse(time.RFC3339, eventTime)
	if err != nil {
		t.Fatal(err)
	}
	if events[2].Name != "scan.complete" || !events[2].Time.Equal(wantTime) {
		t.Fatalf("custom event = {%q %s}, want {scan.complete %s}", events[2].Name, events[2].Time, wantTime)
	}
	assertSpanEventAttribute(t, events[2].Attributes, emissionKindAttribute, emissionKindEvent)
	assertSpanEventAttribute(t, events[2].Attributes, "files", "42")

	if events[3].Name != warningEventName || events[4].Name != warningEventName {
		t.Fatalf("warning events missing: %#v", events[3:])
	}
	assertSpanEventAttribute(t, events[3].Attributes, warningFileAttribute, metricsFile)
	assertSpanEventAttribute(t, events[3].Attributes, warningCountAttribute, "2")
	assertSpanEventAttribute(t, events[4].Attributes, warningFileAttribute, eventsFile)
	assertSpanEventAttribute(t, events[4].Attributes, warningCountAttribute, "1")
}

func TestIngestStageEmissionsUsesLastStageValueWithoutSpan(t *testing.T) {
	dir := PrepareStageTelemetryDir(t.TempDir())
	if err := os.WriteFile(filepath.Join(dir, metricsFile), []byte(
		"{\"name\":\"queue.depth\",\"value\":1}\n"+
			"{\"name\":\"queue.depth\",\"value\":2}\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}

	result := apiv1.ResultEnvelope{}
	IngestStageEmissions(dir, &result, Span{})
	if got := result.Metrics["queue.depth"]; got != 2 {
		t.Fatalf("queue.depth = %v, want last emitted value 2", got)
	}
}

func TestReadEmissionFileContinuesAfterOversizedLine(t *testing.T) {
	dir := PrepareStageTelemetryDir(t.TempDir())
	data := strings.Repeat("x", maxEmissionLineBytes+1) + "\n" +
		"{\"name\":\"after.oversized\",\"value\":3}\n"
	path := filepath.Join(dir, metricsFile)
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}

	records, dropped := readEmissionFile[stageMetric](path, validMetric)
	if len(records) != 1 || records[0].Name != "after.oversized" {
		t.Fatalf("records after oversized line = %#v", records)
	}
	if dropped != 1 {
		t.Fatalf("dropped = %d, want 1", dropped)
	}
}

func assertSpanEventAttribute(t *testing.T, attrs []attribute.KeyValue, key, want string) {
	t.Helper()
	for _, attr := range attrs {
		if string(attr.Key) == key {
			if got := attr.Value.Emit(); got != want {
				t.Fatalf("attribute %q = %q, want %q", key, got, want)
			}
			return
		}
	}
	t.Fatalf("attribute %q missing from %#v", key, attrs)
}
