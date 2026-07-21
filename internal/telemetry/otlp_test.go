package telemetry

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
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
	collectortracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/protobuf/proto"

	"github.com/goobers/goobers/internal/journal"
)

func readOTLPRequests(t *testing.T, dir, runID string) []*collectortracepb.ExportTraceServiceRequest {
	t.Helper()
	path := filepath.Join(dir, runID, spansDirName, otlpFileName)
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = file.Close() }()

	var requests []*collectortracepb.ExportTraceServiceRequest
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		traces, err := (&ptrace.JSONUnmarshaler{}).UnmarshalTraces(scanner.Bytes())
		if err != nil {
			t.Fatalf("decode OTLP/JSON line: %v", err)
		}
		wire, err := (&ptrace.ProtoMarshaler{}).MarshalTraces(traces)
		if err != nil {
			t.Fatalf("encode OTLP protobuf line: %v", err)
		}
		request := new(collectortracepb.ExportTraceServiceRequest)
		if err := proto.Unmarshal(wire, request); err != nil {
			t.Fatalf("decode OTLP protobuf line: %v", err)
		}
		requests = append(requests, request)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan %s: %v", path, err)
	}
	return requests
}

func TestJournalSpanExporterWritesLosslessOTLPJSON(t *testing.T) {
	const (
		runID        = "11111111111111111111111111111111"
		spanID       = "2222222222222222"
		parentSpanID = "3333333333333333"
		linkTraceID  = "44444444444444444444444444444444"
		linkSpanID   = "5555555555555555"
	)
	traceID := mustTraceID(t, runID)
	sid := mustSpanID(t, spanID)
	parentID := mustSpanID(t, parentSpanID)
	linkedTraceID := mustTraceID(t, linkTraceID)
	linkedSpanID := mustSpanID(t, linkSpanID)
	traceState, err := trace.ParseTraceState("vendor=value")
	if err != nil {
		t.Fatal(err)
	}
	linkTraceState, err := trace.ParseTraceState("linked=state")
	if err != nil {
		t.Fatal(err)
	}

	canonical := []attribute.KeyValue{
		attribute.String(AttrRunID, runID),
		attribute.String(AttrGaggle, "alpha"),
		attribute.String(AttrWorkflow, "implementation"),
		attribute.String(AttrWorkflowVersion, "1"),
		attribute.String(AttrWorkflowDigest, "sha256:digest"),
		attribute.String(AttrGoober, "implementer"),
		attribute.String(AttrStage, "implement"),
		attribute.String(AttrStageType, StageTypeAgentic),
		attribute.Int(AttrAttemptNumber, 2),
		attribute.String(AttrAttemptKind, AttemptKindPolicy),
		attribute.String(AttrItemID, "781"),
		attribute.String(AttrItemURL, "https://github.com/Agent-Clubhouse/Goobers/issues/781"),
		attribute.String(AttrOutcome, OutcomeFailure),
		attribute.String(AttrErrorCode, "test-error"),
		attribute.String(AttrGateDecision, "needs-changes"),
		attribute.Int(AttrGateRepassNumber, 1),
		attribute.String(AttrErrorType, "testing"),
		attribute.Int(AttrGenAIUsageInputTokens, 1200),
		attribute.Int(AttrGenAIUsageOutputTokens, 340),
		attribute.Int(AttrCopilotPremiumRequests, 3),
		attribute.Float64(AttrUsageCostUSD, 0.42),
		attribute.String(AttrWorktreeID, runID+"-implement"),
		attribute.String(AttrStorageOperation, "create"),
		attribute.Int(AttrUnmeasuredWorktrees, 0),
		attribute.String(AttrErrorMessage, "fixture error"),
	}
	attrs := append(canonical,
		attribute.Bool("typed.bool", true),
		attribute.Int64("typed.int", 42),
		attribute.Float64("typed.double", 3.5),
		attribute.StringSlice("typed.strings", []string{"one", "two"}),
		attribute.BoolSlice("typed.bools", []bool{true, false}),
		attribute.Int64Slice("typed.ints", []int64{1, 2}),
		attribute.Float64Slice("typed.doubles", []float64{1.25, 2.5}),
	)
	start := time.Unix(1_700_000_000, 123)
	end := start.Add(2 * time.Second)
	scopeAttrs := attribute.NewSet(attribute.Bool("scope.typed", true))
	span := tracetest.SpanStub{
		Name: "task/implement",
		SpanContext: trace.NewSpanContext(trace.SpanContextConfig{
			TraceID: traceID, SpanID: sid, TraceFlags: trace.FlagsSampled, TraceState: traceState,
		}),
		Parent: trace.NewSpanContext(trace.SpanContextConfig{
			TraceID: traceID, SpanID: parentID, Remote: true,
		}),
		SpanKind:   trace.SpanKindConsumer,
		StartTime:  start,
		EndTime:    end,
		Attributes: attrs,
		Events: []sdktrace.Event{{
			Name: "harness.tool_call", Time: start.Add(time.Second),
			Attributes:            []attribute.KeyValue{attribute.Int("event.typed", 7)},
			DroppedAttributeCount: 3,
		}},
		Links: []sdktrace.Link{{
			SpanContext: trace.NewSpanContext(trace.SpanContextConfig{
				TraceID: linkedTraceID, SpanID: linkedSpanID, TraceFlags: trace.FlagsSampled,
				TraceState: linkTraceState, Remote: true,
			}),
			Attributes:            []attribute.KeyValue{attribute.Bool("link.typed", true)},
			DroppedAttributeCount: 4,
		}},
		Status:            sdktrace.Status{Code: codes.Error, Description: "failed"},
		DroppedAttributes: 5,
		DroppedEvents:     6,
		DroppedLinks:      7,
		Resource: resource.NewWithAttributes("https://opentelemetry.io/schemas/1.37.0",
			attribute.String("service.name", "goobers-test"),
			attribute.Int("resource.typed", 9),
		),
		InstrumentationScope: instrumentation.Scope{
			Name:       ScopeName,
			Version:    "1.2.3",
			SchemaURL:  "https://opentelemetry.io/schemas/1.37.0",
			Attributes: scopeAttrs,
		},
	}.Snapshot()

	dir := t.TempDir()
	if err := NewJournalSpanExporter(dir, nil).ExportSpans(t.Context(), []sdktrace.ReadOnlySpan{span}); err != nil {
		t.Fatalf("ExportSpans: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, runID, spansDirName, otlpFileName))
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		ResourceSpans []struct {
			ScopeSpans []struct {
				Spans []struct {
					TraceID      string          `json:"traceId"`
					SpanID       string          `json:"spanId"`
					ParentSpanID string          `json:"parentSpanId"`
					Kind         json.RawMessage `json:"kind"`
					Status       struct {
						Code json.RawMessage `json:"code"`
					} `json:"status"`
				} `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"resourceSpans"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(raw), &wire); err != nil {
		t.Fatalf("decode raw OTLP/JSON: %v", err)
	}
	rawSpan := wire.ResourceSpans[0].ScopeSpans[0].Spans[0]
	if rawSpan.TraceID != runID || rawSpan.SpanID != spanID || rawSpan.ParentSpanID != parentSpanID {
		t.Errorf("raw OTLP/JSON IDs = trace %q span %q parent %q", rawSpan.TraceID, rawSpan.SpanID, rawSpan.ParentSpanID)
	}
	if string(rawSpan.Kind) != "5" || string(rawSpan.Status.Code) != "2" {
		t.Errorf("raw OTLP/JSON enums = kind %s status %s, want 5 and 2", rawSpan.Kind, rawSpan.Status.Code)
	}

	requests := readOTLPRequests(t, dir, runID)
	if len(requests) != 1 {
		t.Fatalf("OTLP request count = %d, want 1", len(requests))
	}
	request := requests[0]
	if got := string(request.ProtoReflect().Descriptor().FullName()); got != OTLPJSONMessageType {
		t.Fatalf("OTLP message type = %q, want %q", got, OTLPJSONMessageType)
	}
	if OTLPJSONContentType != "application/x-ndjson" {
		t.Fatalf("OTLP content type = %q", OTLPJSONContentType)
	}
	if len(request.ResourceSpans) != 1 {
		t.Fatalf("resource spans = %d, want 1", len(request.ResourceSpans))
	}
	resourceSpans := request.ResourceSpans[0]
	if resourceSpans.SchemaUrl != "https://opentelemetry.io/schemas/1.37.0" {
		t.Errorf("resource schema URL = %q", resourceSpans.SchemaUrl)
	}
	resourceAttrs := otlpAttrsByKey(resourceSpans.Resource.Attributes)
	assertOTLPInt(t, resourceAttrs["resource.typed"], 9)
	if len(resourceSpans.ScopeSpans) != 1 {
		t.Fatalf("scope spans = %d, want 1", len(resourceSpans.ScopeSpans))
	}
	scopeSpans := resourceSpans.ScopeSpans[0]
	if scopeSpans.SchemaUrl != "https://opentelemetry.io/schemas/1.37.0" {
		t.Errorf("scope schema URL = %q", scopeSpans.SchemaUrl)
	}
	if scopeSpans.Scope.Name != ScopeName || scopeSpans.Scope.Version != "1.2.3" {
		t.Errorf("scope = %+v", scopeSpans.Scope)
	}
	scopeOTLPAttrs := otlpAttrsByKey(scopeSpans.Scope.Attributes)
	assertOTLPBool(t, scopeOTLPAttrs["scope.typed"], true)
	if len(scopeSpans.Spans) != 1 {
		t.Fatalf("spans = %d, want 1", len(scopeSpans.Spans))
	}

	got := scopeSpans.Spans[0]
	if !bytes.Equal(got.TraceId, traceID[:]) || !bytes.Equal(got.SpanId, sid[:]) || !bytes.Equal(got.ParentSpanId, parentID[:]) {
		t.Errorf("span identity = trace %x span %x parent %x", got.TraceId, got.SpanId, got.ParentSpanId)
	}
	if got.TraceState != traceState.String() || got.Name != "task/implement" ||
		got.Kind.String() != "SPAN_KIND_CONSUMER" ||
		got.StartTimeUnixNano != uint64(start.UnixNano()) ||
		got.EndTimeUnixNano != uint64(end.UnixNano()) {
		t.Errorf("span fields = %+v", got)
	}
	if got.Status.Code.String() != "STATUS_CODE_ERROR" || got.Status.Message != "failed" {
		t.Errorf("status = %+v", got.Status)
	}
	if got.DroppedAttributesCount != 5 || got.DroppedEventsCount != 6 || got.DroppedLinksCount != 7 {
		t.Errorf("dropped counts = attributes %d events %d links %d", got.DroppedAttributesCount, got.DroppedEventsCount, got.DroppedLinksCount)
	}

	spanAttrs := otlpAttrsByKey(got.Attributes)
	for _, key := range AllAttributes() {
		if spanAttrs[string(key)] == nil {
			t.Errorf("canonical attribute %q missing from OTLP span", key)
		}
	}
	assertOTLPInt(t, spanAttrs[AttrAttemptNumber], 2)
	assertOTLPInt(t, spanAttrs[AttrGateRepassNumber], 1)
	assertOTLPBool(t, spanAttrs["typed.bool"], true)
	assertOTLPInt(t, spanAttrs["typed.int"], 42)
	if _, ok := spanAttrs["typed.double"].Value.(*commonpb.AnyValue_DoubleValue); !ok {
		t.Errorf("typed.double type = %T, want double", spanAttrs["typed.double"].Value)
	}
	for _, key := range []string{"typed.strings", "typed.bools", "typed.ints", "typed.doubles"} {
		if _, ok := spanAttrs[key].Value.(*commonpb.AnyValue_ArrayValue); !ok {
			t.Errorf("%s type = %T, want array", key, spanAttrs[key].Value)
		}
	}
	if _, ok := spanAttrs["typed.bools"].GetArrayValue().Values[0].Value.(*commonpb.AnyValue_BoolValue); !ok {
		t.Errorf("typed.bools element type = %T, want bool", spanAttrs["typed.bools"].GetArrayValue().Values[0].Value)
	}

	if len(got.Events) != 1 || got.Events[0].Name != "harness.tool_call" ||
		got.Events[0].TimeUnixNano != uint64(start.Add(time.Second).UnixNano()) ||
		got.Events[0].DroppedAttributesCount != 3 {
		t.Errorf("events = %+v", got.Events)
	} else {
		assertOTLPInt(t, otlpAttrsByKey(got.Events[0].Attributes)["event.typed"], 7)
	}
	if len(got.Links) != 1 || !bytes.Equal(got.Links[0].TraceId, linkedTraceID[:]) ||
		!bytes.Equal(got.Links[0].SpanId, linkedSpanID[:]) ||
		got.Links[0].TraceState != linkTraceState.String() ||
		got.Links[0].DroppedAttributesCount != 4 {
		t.Errorf("links = %+v", got.Links)
	} else {
		assertOTLPBool(t, otlpAttrsByKey(got.Links[0].Attributes)["link.typed"], true)
	}

	if records := readSpanRecords(t, dir, runID); len(records) != 1 {
		t.Fatalf("legacy span records = %d, want 1", len(records))
	}
}

func TestJournalSpanExporterNormalizesInvalidUTF8ForOTLPJSON(t *testing.T) {
	const runID = "66666666666666666666666666666666"
	invalid := "invalid-\xff"
	span := tracetest.SpanStub{
		Name: invalid,
		SpanContext: trace.NewSpanContext(trace.SpanContextConfig{
			TraceID: mustTraceID(t, runID),
			SpanID:  mustSpanID(t, "7777777777777777"),
		}),
		StartTime: time.Unix(1_700_000_000, 0),
		EndTime:   time.Unix(1_700_000_001, 0),
		Attributes: []attribute.KeyValue{
			attribute.String(AttrGaggle, "alpha"),
			attribute.String("invalid.value", invalid),
		},
		Resource:             resource.Empty(),
		InstrumentationScope: instrumentation.Scope{Name: ScopeName},
	}.Snapshot()

	dir := t.TempDir()
	if err := NewJournalSpanExporter(dir, nil).ExportSpans(t.Context(), []sdktrace.ReadOnlySpan{span}); err != nil {
		t.Fatalf("ExportSpans: %v", err)
	}

	requests := readOTLPRequests(t, dir, runID)
	if len(requests) != 1 {
		t.Fatalf("OTLP request count = %d, want 1", len(requests))
	}
	got := requests[0].ResourceSpans[0].ScopeSpans[0].Spans[0]
	want := strings.ToValidUTF8(invalid, "\uFFFD")
	if got.Name != want || otlpAttrsByKey(got.Attributes)["invalid.value"].GetStringValue() != want {
		t.Errorf("normalized OTLP span = name %q attributes %+v, want %q", got.Name, got.Attributes, want)
	}
	if records := readSpanRecords(t, dir, runID); len(records) != 1 || records[0].Name != want {
		t.Errorf("legacy span records = %+v, want one normalized record", records)
	}
}

func TestJournalSpanExporterRedactsFloatWireRepresentation(t *testing.T) {
	const (
		runID  = "88888888888888888888888888888888"
		secret = "1000000"
	)
	registry := journal.NewRegistryScrubber()
	registry.Register([]byte(secret))
	at := time.Unix(2, 345)
	span := tracetest.SpanStub{
		Name: "float-redaction",
		SpanContext: trace.NewSpanContext(trace.SpanContextConfig{
			TraceID: mustTraceID(t, runID),
			SpanID:  mustSpanID(t, "9999999999999999"),
		}),
		StartTime: at,
		EndTime:   at,
		Attributes: []attribute.KeyValue{
			attribute.Float64("scalar", 1_000_000),
			attribute.Float64Slice("slice", []float64{1_000_000}),
		},
		Resource:             resource.Empty(),
		InstrumentationScope: instrumentation.Scope{Name: ScopeName},
	}.Snapshot()

	dir := t.TempDir()
	if err := NewJournalSpanExporter(dir, registry).ExportSpans(t.Context(), []sdktrace.ReadOnlySpan{span}); err != nil {
		t.Fatalf("ExportSpans: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, runID, spansDirName, otlpFileName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("registered float secret leaked in its OTLP/JSON wire representation: %s", raw)
	}

	got := readOTLPRequests(t, dir, runID)[0].ResourceSpans[0].ScopeSpans[0].Spans[0]
	attrs := otlpAttrsByKey(got.Attributes)
	if attrs["scalar"].GetStringValue() != RedactedPlaceholder {
		t.Errorf("scalar float = %v, want redacted string", attrs["scalar"])
	}
	values := attrs["slice"].GetArrayValue().Values
	if len(values) != 1 || values[0].GetStringValue() != RedactedPlaceholder {
		t.Errorf("float slice = %v, want redacted string element", values)
	}
}

func mustTraceID(t *testing.T, value string) trace.TraceID {
	t.Helper()
	id, err := trace.TraceIDFromHex(value)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func mustSpanID(t *testing.T, value string) trace.SpanID {
	t.Helper()
	id, err := trace.SpanIDFromHex(value)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func otlpAttrsByKey(attrs []*commonpb.KeyValue) map[string]*commonpb.AnyValue {
	out := make(map[string]*commonpb.AnyValue, len(attrs))
	for _, attr := range attrs {
		out[attr.Key] = attr.Value
	}
	return out
}

func assertOTLPBool(t *testing.T, value *commonpb.AnyValue, want bool) {
	t.Helper()
	if value == nil {
		t.Fatal("OTLP attribute is missing")
	}
	if _, ok := value.Value.(*commonpb.AnyValue_BoolValue); !ok || value.GetBoolValue() != want {
		t.Errorf("OTLP attribute = %T(%v), want bool(%v)", value.Value, value, want)
	}
}

func assertOTLPInt(t *testing.T, value *commonpb.AnyValue, want int64) {
	t.Helper()
	if value == nil {
		t.Fatal("OTLP attribute is missing")
	}
	if _, ok := value.Value.(*commonpb.AnyValue_IntValue); !ok || value.GetIntValue() != want {
		t.Errorf("OTLP attribute = %T(%v), want int(%d)", value.Value, value, want)
	}
}
