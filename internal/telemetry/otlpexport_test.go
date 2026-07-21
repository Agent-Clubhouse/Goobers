package telemetry

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestExportJournalOTLPUsesInclusiveExclusiveSpanStartWindow(t *testing.T) {
	since := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	until := since.Add(time.Hour)
	spans := []sdktrace.ReadOnlySpan{
		exportFixtureSpan(t, "11111111111111111111111111111111", "0000000000000001", "before", since.Add(-time.Nanosecond)),
		exportFixtureSpan(t, "11111111111111111111111111111111", "0000000000000002", "since", since),
		exportFixtureSpan(t, "11111111111111111111111111111111", "0000000000000003", "inside", until.Add(-time.Nanosecond)),
		exportFixtureSpan(t, "11111111111111111111111111111111", "0000000000000004", "until", until),
	}
	runsDir := t.TempDir()
	if err := NewJournalSpanExporter(runsDir, nil).ExportSpans(t.Context(), spans); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := ExportJournalOTLP([]string{runsDir}, since, until, &out); err != nil {
		t.Fatal(err)
	}
	if got, want := exportedSpanNames(t, out.Bytes()), []string{"since", "inside"}; !slices.Equal(got, want) {
		t.Fatalf("exported spans = %v, want %v", got, want)
	}
}

func TestExportJournalOTLPMatchesLiveEmission(t *testing.T) {
	since := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)
	spans := []sdktrace.ReadOnlySpan{
		exportFixtureSpan(t, "22222222222222222222222222222222", "0000000000000001", "before", since.Add(-time.Nanosecond)),
		exportFixtureSpan(t, "22222222222222222222222222222222", "0000000000000002", "selected", since),
	}
	exporter := NewJournalSpanExporter(t.TempDir(), nil)
	live, err := exporter.marshalOTLP(spans[1:])
	if err != nil {
		t.Fatal(err)
	}
	if err := exporter.ExportSpans(t.Context(), spans); err != nil {
		t.Fatal(err)
	}

	var backfill bytes.Buffer
	if err := ExportJournalOTLP([]string{exporter.runsDir}, since, since.Add(time.Hour), &backfill); err != nil {
		t.Fatal(err)
	}
	if want := append(live, '\n'); !bytes.Equal(backfill.Bytes(), want) {
		t.Fatalf("backfilled OTLP differs from live emission\nbackfill: %s\nlive: %s", backfill.Bytes(), want)
	}
}

func TestExportJournalOTLPRejectsInvalidJournalData(t *testing.T) {
	tests := []struct {
		name      string
		contents  *string
		wantError string
	}{
		{name: "missing", wantError: "journaled OTLP data is missing"},
		{name: "empty", contents: stringPointer(""), wantError: "contains no records"},
		{name: "corrupt", contents: stringPointer("{\n"), wantError: "corrupt OTLP journal record 1"},
		{name: "unsupported", contents: stringPointer("{\"resourceMetrics\":[]}\n"), wantError: "unsupported OTLP journal record 1"},
		{name: "truncated", contents: stringPointer("{\"resourceSpans\":[]}"), wantError: "missing newline delimiter"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runsDir := t.TempDir()
			path := filepath.Join(runsDir, "fixture-run", spansDirName, otlpFileName)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if test.contents != nil {
				if err := os.WriteFile(path, []byte(*test.contents), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			var out bytes.Buffer
			err := ExportJournalOTLP(
				[]string{runsDir},
				time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC),
				time.Time{},
				&out,
			)
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("error = %v, want containing %q", err, test.wantError)
			}
		})
	}
}

func exportFixtureSpan(t *testing.T, traceID, spanID, name string, start time.Time) sdktrace.ReadOnlySpan {
	t.Helper()
	return tracetest.SpanStub{
		Name: name,
		SpanContext: trace.NewSpanContext(trace.SpanContextConfig{
			TraceID: mustTraceID(t, traceID),
			SpanID:  mustSpanID(t, spanID),
		}),
		StartTime:  start,
		EndTime:    start.Add(time.Millisecond),
		Attributes: []attribute.KeyValue{attribute.String("fixture.name", name)},
		Events: []sdktrace.Event{{
			Name: "fixture.event",
			Time: start.Add(time.Microsecond),
			Attributes: []attribute.KeyValue{
				attribute.Bool("fixture.event.accepted", true),
			},
		}},
		Status:               sdktrace.Status{Code: codes.Ok, Description: "selected"},
		Resource:             resource.NewWithAttributes("", attribute.String("fixture.resource", "test")),
		InstrumentationScope: instrumentation.Scope{Name: ScopeName},
	}.Snapshot()
}

func exportedSpanNames(t *testing.T, data []byte) []string {
	t.Helper()
	var names []string
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		traces, err := (&ptrace.JSONUnmarshaler{}).UnmarshalTraces(line)
		if err != nil {
			t.Fatal(err)
		}
		resourceSpans := traces.ResourceSpans()
		for i := 0; i < resourceSpans.Len(); i++ {
			scopeSpans := resourceSpans.At(i).ScopeSpans()
			for j := 0; j < scopeSpans.Len(); j++ {
				spans := scopeSpans.At(j).Spans()
				for k := 0; k < spans.Len(); k++ {
					names = append(names, spans.At(k).Name())
				}
			}
		}
	}
	return names
}

func stringPointer(value string) *string {
	return &value
}
