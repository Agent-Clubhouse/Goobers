package telemetry

import (
	"errors"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Span is a small handle for ending or annotating a Goobers telemetry span.
type Span struct {
	span trace.Span
}

// End finishes the span without changing its status.
func (s Span) End() {
	if s.span != nil {
		s.span.End()
	}
}

// Succeed marks the span as successful and finishes it.
func (s Span) Succeed(message string) {
	if s.span == nil {
		return
	}
	s.span.SetStatus(codes.Ok, message)
	s.span.End()
}

// Fail records err, marks the span as failed, and finishes it.
func (s Span) Fail(err error) {
	if s.span == nil {
		return
	}
	if err == nil {
		err = errors.New("span failed")
	}
	s.span.RecordError(err)
	s.span.SetStatus(codes.Error, err.Error())
	s.span.End()
}

// Event records a named span event.
func (s Span) Event(name string, attrs ...attribute.KeyValue) {
	if s.span != nil {
		s.span.AddEvent(name, trace.WithAttributes(attrs...))
	}
}

func runSpanName(workflowID string) string {
	return "run/" + workflowID
}

func taskSpanName(taskID string) string {
	return "task/" + taskID
}

func gateSpanName(gateID string) string {
	return "gate/" + gateID
}

func schedulerSpanName(action string) string {
	return "scheduler/" + action
}
