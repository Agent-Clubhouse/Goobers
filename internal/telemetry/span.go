package telemetry

import (
	"errors"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/goobers/goobers/internal/journal"
)

// Span is a small handle for ending or annotating a Goobers telemetry span.
type Span struct {
	span     trace.Span
	scrubber journal.Scrubber
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
	s.span.SetAttributes(attribute.String(AttrOutcome, OutcomeSuccess))
	s.span.SetStatus(codes.Ok, s.scrub(message))
	s.span.End()
}

// Complete finishes a span with a business outcome.
func (s Span) Complete(outcome string, isFailure bool) {
	s.CompleteWithError(outcome, "", isFailure)
}

// CompleteWithError finishes a span and records a journal-aligned error code.
func (s Span) CompleteWithError(outcome, errorCode string, isFailure bool) {
	if s.span == nil {
		return
	}
	outcome = s.scrub(outcome)
	attrs := []attribute.KeyValue{attribute.String(AttrOutcome, outcome)}
	if errorCode != "" {
		errorCode = s.scrub(errorCode)
		attrs = append(attrs, attribute.String(AttrErrorCode, errorCode))
	}
	if isFailure {
		errorType := errorCode
		if errorType == "" {
			errorType = outcome
		}
		attrs = append(attrs, attribute.String(AttrErrorType, errorType))
	}
	s.span.SetAttributes(attrs...)
	if isFailure {
		s.span.SetStatus(codes.Error, outcome)
	} else {
		s.span.SetStatus(codes.Ok, outcome)
	}
	s.span.End()
}

// Fail records err, marks the span as failed, and finishes it.
func (s Span) Fail(err error) {
	s.FailWithCode(err, "")
}

// FailWithCode records an operational failure and its journal error code.
func (s Span) FailWithCode(err error, errorCode string) {
	if s.span == nil {
		return
	}
	if err == nil {
		err = errors.New("span failed")
	}
	message := s.scrub(err.Error())
	errorType := fmt.Sprintf("%T", err)
	if errorCode != "" {
		errorType = errorCode
	}
	attrs := []attribute.KeyValue{
		attribute.String(AttrOutcome, OutcomeFailure),
		attribute.String(AttrErrorType, s.scrub(errorType)),
	}
	if errorCode != "" {
		errorCode = s.scrub(errorCode)
		attrs = append(attrs,
			attribute.String(AttrErrorCode, errorCode),
		)
	}
	s.span.SetAttributes(attrs...)
	s.span.RecordError(errors.New(message))
	s.span.SetStatus(codes.Error, message)
	s.span.End()
}

// SetGateResult records the decision and repass count from gate.evaluated.
func (s Span) SetGateResult(decision string, repassNumber int) {
	if s.span == nil {
		return
	}
	s.span.SetAttributes(
		attribute.String(AttrGateDecision, s.scrub(decision)),
		attribute.Int(AttrGateRepassNumber, repassNumber),
	)
}

// Event records a named span event.
func (s Span) Event(name string, attrs ...attribute.KeyValue) {
	if s.span != nil {
		s.span.AddEvent(s.scrub(name), trace.WithAttributes(scrubAttributes(s.scrubber, attrs)...))
	}
}

// EventAt records a named span event with the timestamp supplied by the stage.
func (s Span) EventAt(at time.Time, name string, attrs ...attribute.KeyValue) {
	if s.span != nil {
		s.span.AddEvent(s.scrub(name), trace.WithTimestamp(at), trace.WithAttributes(scrubAttributes(s.scrubber, attrs)...))
	}
}

func (s Span) scrub(value string) string {
	if s.scrubber == nil {
		return redactString(value)
	}
	return redactWith(s.scrubber, value)
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
