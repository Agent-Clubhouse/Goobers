package telemetry

import (
	"errors"
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
	s.span.SetStatus(codes.Ok, s.scrub(message))
	s.span.End()
}

// Complete finishes the span with a business-level outcome — a run/stage
// result the executor reported cleanly (a dispatch success), which is NOT the
// same axis as Succeed/Fail's genuine dispatch-success/infra-error meaning
// (issue #710). businessStatus is recorded as a goobers.business_status span
// attribute (AttrBusinessStatus) — captured into spans.jsonl's generic
// Attributes map by JournalSpanExporter with no exporter changes needed — so
// rollup/trace consumers can query the actual outcome. isFailure additionally
// sets the OTel span status to codes.Error: before this, a business failure
// (a task's ResultEnvelope status "failure", a run's terminal PhaseFailed)
// called span.Succeed(status) unconditionally, reporting codes.Ok with the
// literal string "failed" as its message — every span-based view (`goobers
// trace`, rollup span queries) then read a died run as "ok", the exact gap
// that made #705 a 16-hour mystery despite the real cause sitting one journal
// line away in stage.finished the whole time. Every other business status
// (success, completed, aborted, escalated, blocked's pre-#544 pause) keeps
// codes.Ok — this only recategorizes the one outcome that was actively lying.
func (s Span) Complete(businessStatus string, isFailure bool) {
	if s.span == nil {
		return
	}
	businessStatus = s.scrub(businessStatus)
	s.span.SetAttributes(attribute.String(AttrBusinessStatus, businessStatus))
	if isFailure {
		s.span.SetStatus(codes.Error, businessStatus)
	} else {
		s.span.SetStatus(codes.Ok, businessStatus)
	}
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
	message := s.scrub(err.Error())
	s.span.RecordError(errors.New(message))
	s.span.SetStatus(codes.Error, message)
	s.span.End()
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
