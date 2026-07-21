package telemetry

import (
	"math"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	collectortracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

func (e *JournalSpanExporter) marshalOTLP(spans []sdktrace.ReadOnlySpan) ([]byte, error) {
	request := &collectortracepb.ExportTraceServiceRequest{
		ResourceSpans: e.otlpResourceSpans(spans),
	}
	wire, err := proto.Marshal(request)
	if err != nil {
		return nil, err
	}
	traces, err := (&ptrace.ProtoUnmarshaler{}).UnmarshalTraces(wire)
	if err != nil {
		return nil, err
	}
	record, err := (&ptrace.JSONMarshaler{}).MarshalTraces(traces)
	if err != nil {
		return nil, err
	}
	return record, nil
}

// The SDK's OTLP transform is internal to its exporter module. Keep this
// conversion at the same ReadOnlySpan boundary so the journal never depends on
// the lossy legacy SpanRecord projection.
func (e *JournalSpanExporter) otlpResourceSpans(spans []sdktrace.ReadOnlySpan) []*tracepb.ResourceSpans {
	type scopeKey struct {
		resource attribute.Distinct
		scope    instrumentation.Scope
	}

	resourceSpans := make(map[attribute.Distinct]*tracepb.ResourceSpans)
	scopeSpans := make(map[scopeKey]*tracepb.ScopeSpans)
	for _, span := range spans {
		if span == nil {
			continue
		}

		resourceKey := span.Resource().Equivalent()
		key := scopeKey{resource: resourceKey, scope: span.InstrumentationScope()}
		scope, scopeExists := scopeSpans[key]
		if !scopeExists {
			scope = &tracepb.ScopeSpans{
				Scope:     e.otlpScope(span.InstrumentationScope()),
				SchemaUrl: e.otlpString(span.InstrumentationScope().SchemaURL),
			}
			scopeSpans[key] = scope
		}
		scope.Spans = append(scope.Spans, e.otlpSpan(span))

		resource, resourceExists := resourceSpans[resourceKey]
		if !resourceExists {
			resource = &tracepb.ResourceSpans{
				Resource: &resourcepb.Resource{
					Attributes: e.otlpAttributes(span.Resource().Attributes()),
				},
				SchemaUrl: e.otlpString(span.Resource().SchemaURL()),
			}
			resourceSpans[resourceKey] = resource
		}
		if !scopeExists {
			resource.ScopeSpans = append(resource.ScopeSpans, scope)
		}
	}

	out := make([]*tracepb.ResourceSpans, 0, len(resourceSpans))
	for _, resource := range resourceSpans {
		out = append(out, resource)
	}
	return out
}

func (e *JournalSpanExporter) otlpScope(scope instrumentation.Scope) *commonpb.InstrumentationScope {
	if scope == (instrumentation.Scope{}) {
		return nil
	}
	return &commonpb.InstrumentationScope{
		Name:       e.otlpString(scope.Name),
		Version:    e.otlpString(scope.Version),
		Attributes: e.otlpAttributes(scope.Attributes.ToSlice()),
	}
}

func (e *JournalSpanExporter) otlpSpan(span sdktrace.ReadOnlySpan) *tracepb.Span {
	spanContext := span.SpanContext()
	traceID := spanContext.TraceID()
	spanID := spanContext.SpanID()
	out := &tracepb.Span{
		TraceId:                traceID[:],
		SpanId:                 spanID[:],
		TraceState:             e.otlpString(spanContext.TraceState().String()),
		Name:                   e.otlpString(span.Name()),
		Kind:                   otlpSpanKind(span.SpanKind()),
		StartTimeUnixNano:      nonNegativeUnixNano(span.StartTime()),
		EndTimeUnixNano:        nonNegativeUnixNano(span.EndTime()),
		Attributes:             e.otlpAttributes(span.Attributes()),
		DroppedAttributesCount: clampUint32(span.DroppedAttributes()),
		Events:                 e.otlpEvents(span.Events()),
		DroppedEventsCount:     clampUint32(span.DroppedEvents()),
		Links:                  e.otlpLinks(span.Links()),
		DroppedLinksCount:      clampUint32(span.DroppedLinks()),
		Status: &tracepb.Status{
			Message: e.otlpString(span.Status().Description),
			Code:    otlpStatusCode(span.Status().Code),
		},
		Flags: otlpSpanFlags(spanContext.TraceFlags(), span.Parent()),
	}
	if parentSpanID := span.Parent().SpanID(); parentSpanID.IsValid() {
		out.ParentSpanId = parentSpanID[:]
	}
	return out
}

func (e *JournalSpanExporter) otlpEvents(events []sdktrace.Event) []*tracepb.Span_Event {
	out := make([]*tracepb.Span_Event, len(events))
	for i, event := range events {
		out[i] = &tracepb.Span_Event{
			TimeUnixNano:           nonNegativeUnixNano(event.Time),
			Name:                   e.otlpString(event.Name),
			Attributes:             e.otlpAttributes(event.Attributes),
			DroppedAttributesCount: clampUint32(event.DroppedAttributeCount),
		}
	}
	return out
}

func (e *JournalSpanExporter) otlpLinks(links []sdktrace.Link) []*tracepb.Span_Link {
	out := make([]*tracepb.Span_Link, len(links))
	for i, link := range links {
		traceID := link.SpanContext.TraceID()
		spanID := link.SpanContext.SpanID()
		out[i] = &tracepb.Span_Link{
			TraceId:                traceID[:],
			SpanId:                 spanID[:],
			TraceState:             e.otlpString(link.SpanContext.TraceState().String()),
			Attributes:             e.otlpAttributes(link.Attributes),
			DroppedAttributesCount: clampUint32(link.DroppedAttributeCount),
			Flags:                  otlpSpanFlags(link.SpanContext.TraceFlags(), link.SpanContext),
		}
	}
	return out
}

func (e *JournalSpanExporter) otlpAttributes(attrs []attribute.KeyValue) []*commonpb.KeyValue {
	if len(attrs) == 0 {
		return nil
	}
	out := make([]*commonpb.KeyValue, len(attrs))
	for i, attr := range attrs {
		out[i] = &commonpb.KeyValue{
			Key:   e.otlpString(string(attr.Key)),
			Value: e.otlpAttributeValue(attr.Value),
		}
	}
	return out
}

func (e *JournalSpanExporter) otlpAttributeValue(value attribute.Value) *commonpb.AnyValue {
	out := new(commonpb.AnyValue)
	switch value.Type() {
	case attribute.BOOL:
		if redacted, changed := e.otlpRedacted(strconv.FormatBool(value.AsBool())); changed {
			out.Value = &commonpb.AnyValue_StringValue{StringValue: redacted}
		} else {
			out.Value = &commonpb.AnyValue_BoolValue{BoolValue: value.AsBool()}
		}
	case attribute.BOOLSLICE:
		out.Value = &commonpb.AnyValue_ArrayValue{ArrayValue: e.otlpBoolSlice(value.AsBoolSlice())}
	case attribute.INT64:
		if redacted, changed := e.otlpRedacted(strconv.FormatInt(value.AsInt64(), 10)); changed {
			out.Value = &commonpb.AnyValue_StringValue{StringValue: redacted}
		} else {
			out.Value = &commonpb.AnyValue_IntValue{IntValue: value.AsInt64()}
		}
	case attribute.INT64SLICE:
		out.Value = &commonpb.AnyValue_ArrayValue{ArrayValue: e.otlpInt64Slice(value.AsInt64Slice())}
	case attribute.FLOAT64:
		if redacted, changed := e.otlpRedacted(otlpJSONFloat(value.AsFloat64())); changed {
			out.Value = &commonpb.AnyValue_StringValue{StringValue: redacted}
		} else {
			out.Value = &commonpb.AnyValue_DoubleValue{DoubleValue: value.AsFloat64()}
		}
	case attribute.FLOAT64SLICE:
		out.Value = &commonpb.AnyValue_ArrayValue{ArrayValue: e.otlpFloat64Slice(value.AsFloat64Slice())}
	case attribute.STRING:
		out.Value = &commonpb.AnyValue_StringValue{StringValue: e.otlpString(value.AsString())}
	case attribute.STRINGSLICE:
		out.Value = &commonpb.AnyValue_ArrayValue{ArrayValue: e.otlpStringSlice(value.AsStringSlice())}
	default:
		out.Value = &commonpb.AnyValue_StringValue{StringValue: "INVALID"}
	}
	return out
}

func (e *JournalSpanExporter) otlpBoolSlice(values []bool) *commonpb.ArrayValue {
	out := &commonpb.ArrayValue{Values: make([]*commonpb.AnyValue, len(values))}
	for i, value := range values {
		if redacted, changed := e.otlpRedacted(strconv.FormatBool(value)); changed {
			out.Values[i] = &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: redacted}}
		} else {
			out.Values[i] = &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: value}}
		}
	}
	return out
}

func (e *JournalSpanExporter) otlpInt64Slice(values []int64) *commonpb.ArrayValue {
	out := &commonpb.ArrayValue{Values: make([]*commonpb.AnyValue, len(values))}
	for i, value := range values {
		if redacted, changed := e.otlpRedacted(strconv.FormatInt(value, 10)); changed {
			out.Values[i] = &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: redacted}}
		} else {
			out.Values[i] = &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: value}}
		}
	}
	return out
}

func (e *JournalSpanExporter) otlpFloat64Slice(values []float64) *commonpb.ArrayValue {
	out := &commonpb.ArrayValue{Values: make([]*commonpb.AnyValue, len(values))}
	for i, value := range values {
		if redacted, changed := e.otlpRedacted(otlpJSONFloat(value)); changed {
			out.Values[i] = &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: redacted}}
		} else {
			out.Values[i] = &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: value}}
		}
	}
	return out
}

func (e *JournalSpanExporter) otlpStringSlice(values []string) *commonpb.ArrayValue {
	out := &commonpb.ArrayValue{Values: make([]*commonpb.AnyValue, len(values))}
	for i, value := range values {
		out.Values[i] = &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: e.otlpString(value)}}
	}
	return out
}

func (e *JournalSpanExporter) otlpString(value string) string {
	redacted, _ := e.otlpRedacted(value)
	return redacted
}

func (e *JournalSpanExporter) otlpRedacted(value string) (string, bool) {
	redacted := redactWith(e.scrubber, value)
	return strings.ToValidUTF8(redacted, "\uFFFD"), redacted != value
}

// OTLP/JSON uses decimal notation between 1e-6 and 1e21, unlike
// attribute.Value.Emit, which renders values such as 1000000 in exponent form.
func otlpJSONFloat(value float64) string {
	switch {
	case math.IsNaN(value):
		return "NaN"
	case math.IsInf(value, 1):
		return "Infinity"
	case math.IsInf(value, -1):
		return "-Infinity"
	}
	format := byte('f')
	abs := math.Abs(value)
	if abs != 0 && (abs < 1e-6 || abs >= 1e21) {
		format = 'e'
	}
	return strconv.FormatFloat(value, format, -1, 64)
}

func otlpSpanKind(kind trace.SpanKind) tracepb.Span_SpanKind {
	switch kind {
	case trace.SpanKindInternal:
		return tracepb.Span_SPAN_KIND_INTERNAL
	case trace.SpanKindServer:
		return tracepb.Span_SPAN_KIND_SERVER
	case trace.SpanKindClient:
		return tracepb.Span_SPAN_KIND_CLIENT
	case trace.SpanKindProducer:
		return tracepb.Span_SPAN_KIND_PRODUCER
	case trace.SpanKindConsumer:
		return tracepb.Span_SPAN_KIND_CONSUMER
	default:
		return tracepb.Span_SPAN_KIND_UNSPECIFIED
	}
}

func otlpStatusCode(code codes.Code) tracepb.Status_StatusCode {
	switch code {
	case codes.Ok:
		return tracepb.Status_STATUS_CODE_OK
	case codes.Error:
		return tracepb.Status_STATUS_CODE_ERROR
	default:
		return tracepb.Status_STATUS_CODE_UNSET
	}
}

func otlpSpanFlags(flags trace.TraceFlags, parent trace.SpanContext) uint32 {
	out := uint32(flags) | uint32(tracepb.SpanFlags_SPAN_FLAGS_CONTEXT_HAS_IS_REMOTE_MASK)
	if parent.IsRemote() {
		out |= uint32(tracepb.SpanFlags_SPAN_FLAGS_CONTEXT_IS_REMOTE_MASK)
	}
	return out
}

func clampUint32(value int) uint32 {
	switch {
	case value < 0:
		return 0
	case int64(value) > math.MaxUint32:
		return math.MaxUint32
	default:
		return uint32(value)
	}
}

func nonNegativeUnixNano(value time.Time) uint64 {
	nanos := value.UnixNano()
	if nanos < 0 {
		return 0
	}
	return uint64(nanos)
}
