package telemetry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

type requestedTraceIDKey struct{}

type runIDGenerator struct{}

var _ sdktrace.IDGenerator = runIDGenerator{}

func (g runIDGenerator) NewIDs(ctx context.Context) (trace.TraceID, trace.SpanID) {
	if traceID, ok := ctx.Value(requestedTraceIDKey{}).(trace.TraceID); ok && traceID.IsValid() {
		return traceID, randomSpanID()
	}
	return randomTraceID(), randomSpanID()
}

func (g runIDGenerator) NewSpanID(context.Context, trace.TraceID) trace.SpanID {
	return randomSpanID()
}

func contextWithRequestedTraceID(ctx context.Context, traceID trace.TraceID) context.Context {
	return context.WithValue(ctx, requestedTraceIDKey{}, traceID)
}

func contextWithRunTraceID(ctx context.Context, runID string) (context.Context, error) {
	traceID, err := parseTraceID(runID)
	if err != nil {
		return ctx, err
	}
	parent := trace.SpanContextFromContext(ctx)
	if parent.IsValid() && parent.TraceID() != traceID {
		return ctx, fmt.Errorf("run id %q does not match parent trace id %q", runID, parent.TraceID())
	}
	return contextWithRequestedTraceID(ctx, traceID), nil
}

func parseTraceID(runID string) (trace.TraceID, error) {
	traceID, err := trace.TraceIDFromHex(runID)
	if err != nil {
		return trace.TraceID{}, fmt.Errorf("run id must be a 32-character OpenTelemetry trace id: %w", err)
	}
	if !traceID.IsValid() {
		return trace.TraceID{}, fmt.Errorf("run id %q is not a valid OpenTelemetry trace id", runID)
	}
	return traceID, nil
}

func randomTraceID() trace.TraceID {
	var b [16]byte
	for {
		if _, err := rand.Read(b[:]); err != nil {
			panic(fmt.Errorf("generate trace id: %w", err))
		}
		traceID, err := trace.TraceIDFromHex(hex.EncodeToString(b[:]))
		if err == nil && traceID.IsValid() {
			return traceID
		}
	}
}

func randomSpanID() trace.SpanID {
	var b [8]byte
	for {
		if _, err := rand.Read(b[:]); err != nil {
			panic(fmt.Errorf("generate span id: %w", err))
		}
		spanID, err := trace.SpanIDFromHex(hex.EncodeToString(b[:]))
		if err == nil && spanID.IsValid() {
			return spanID
		}
	}
}
