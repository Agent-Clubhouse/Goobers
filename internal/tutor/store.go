package tutor

import (
	"context"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/goobers/goobers/internal/telemetry"
)

const (
	attrRetryCount = "retry.count"
	attrRetries    = "retries"
	statusOK       = "ok"
	statusError    = "error"
)

// SpanStore adapts completed OpenTelemetry spans into Tutor signals.
type SpanStore struct {
	spans []sdktrace.ReadOnlySpan
}

// NewSpanStore snapshots OTel spans exported by the Goobers telemetry layer.
func NewSpanStore(spans []sdktrace.ReadOnlySpan) SpanStore {
	copied := make([]sdktrace.ReadOnlySpan, len(spans))
	copy(copied, spans)
	return SpanStore{spans: copied}
}

// QuerySignals returns run/task/gate signals matching the query.
func (s SpanStore) QuerySignals(ctx context.Context, q Query) ([]Signal, error) {
	signals := make([]Signal, 0, len(s.spans))
	for _, span := range s.spans {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		sig, ok := signalFromSpan(span)
		if !ok || !matchesQuery(sig, q) {
			continue
		}
		signals = append(signals, sig)
	}
	return signals, nil
}

func signalFromSpan(span sdktrace.ReadOnlySpan) (Signal, bool) {
	attrs := attrStrings(span.Attributes())
	kind := SignalKind(attrs[telemetry.AttrSpanKind])
	if kind != SignalRun && kind != SignalTask && kind != SignalGate {
		return Signal{}, false
	}
	status := ""
	switch span.Status().Code {
	case codes.Ok:
		status = statusOK
	case codes.Error:
		status = statusError
	}
	end := span.EndTime()
	if end.IsZero() {
		end = span.StartTime()
	}
	return Signal{
		Kind:            kind,
		Gaggle:          attrs[telemetry.AttrGaggle],
		WorkflowID:      attrs[telemetry.AttrWorkflowID],
		WorkflowVersion: attrs[telemetry.AttrWorkflowVersion],
		RunID:           attrs[telemetry.AttrRunID],
		TaskID:          attrs[telemetry.AttrTaskID],
		GateID:          attrs[telemetry.AttrGateID],
		GooberID:        attrs[telemetry.AttrGooberID],
		Decision:        attrs[telemetry.AttrGateDecision],
		Status:          status,
		Error:           span.Status().Description,
		Duration:        end.Sub(span.StartTime()),
		RetryCount:      retryCount(span),
		ObservedAt:      end,
	}, true
}

func matchesQuery(sig Signal, q Query) bool {
	if q.Gaggle != "" && sig.Gaggle != q.Gaggle {
		return false
	}
	if !q.Since.IsZero() && sig.ObservedAt.Before(q.Since) {
		return false
	}
	if !q.Until.IsZero() && sig.ObservedAt.After(q.Until) {
		return false
	}
	return true
}

func attrStrings(attrs []attribute.KeyValue) map[string]string {
	out := make(map[string]string, len(attrs))
	for _, attr := range attrs {
		out[string(attr.Key)] = attrValueString(attr.Value)
	}
	return out
}

func attrValueString(value attribute.Value) string {
	switch value.Type() {
	case attribute.STRING:
		return value.AsString()
	case attribute.BOOL:
		return strconv.FormatBool(value.AsBool())
	case attribute.INT64:
		return strconv.FormatInt(value.AsInt64(), 10)
	case attribute.FLOAT64:
		return strconv.FormatFloat(value.AsFloat64(), 'f', -1, 64)
	default:
		return value.Emit()
	}
}

func retryCount(span sdktrace.ReadOnlySpan) int {
	count := retryCountFromAttrs(span.Attributes())
	for _, event := range span.Events() {
		name := strings.ToLower(event.Name)
		if strings.Contains(name, "retry") {
			count++
		}
		if eventCount := retryCountFromAttrs(event.Attributes); eventCount > count {
			count = eventCount
		}
	}
	return count
}

func retryCountFromAttrs(attrs []attribute.KeyValue) int {
	maxCount := 0
	for _, attr := range attrs {
		key := string(attr.Key)
		if key != attrRetryCount && key != attrRetries {
			continue
		}
		var count int
		if attr.Value.Type() == attribute.INT64 {
			count = int(attr.Value.AsInt64())
		} else if parsed, err := strconv.Atoi(attr.Value.AsString()); err == nil {
			count = parsed
		}
		if count > maxCount {
			maxCount = count
		}
	}
	return maxCount
}

func evidenceFor(sig Signal) Evidence {
	id := sig.TaskID
	if sig.Kind == SignalGate {
		id = sig.GateID
	}
	return Evidence{
		RunID:      sig.RunID,
		Signal:     string(sig.Kind) + "/" + id,
		Status:     sig.Status,
		Decision:   sig.Decision,
		Duration:   sig.Duration,
		RetryCount: sig.RetryCount,
		URL:        sig.EvidenceURL,
		ObservedAt: sig.ObservedAt,
	}
}

var _ TelemetryStore = SpanStore{}

// StaticStore is a deterministic TelemetryStore useful for tests and dry runs.
type StaticStore []Signal

// QuerySignals returns matching static signals.
func (s StaticStore) QuerySignals(ctx context.Context, q Query) ([]Signal, error) {
	out := make([]Signal, 0, len(s))
	for _, sig := range s {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		if matchesQuery(sig, q) {
			out = append(out, sig)
		}
	}
	return out, nil
}

var _ TelemetryStore = StaticStore{}
