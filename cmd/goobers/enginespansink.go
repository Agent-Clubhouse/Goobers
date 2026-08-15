package main

import (
	"context"
	"time"

	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/telemetry"
)

// engineSpanSink adapts the daemon's telemetry client to engine.SpanSink, so a
// projected tier-3 run emits the same span shapes a tier-1 run does.
//
// The adapter lives here rather than in internal/engine because the engine
// deliberately holds no dependency on the telemetry implementation — it
// declares the narrow interface it needs and the wiring site satisfies it.
type engineSpanSink struct{ client *telemetry.Client }

// newEngineSpanSink returns nil when telemetry is disabled, which
// engine.SynthesizeRunSpans treats as a no-op rather than an error.
func newEngineSpanSink(client *telemetry.Client) engine.SpanSink {
	if client == nil {
		return nil
	}
	return engineSpanSink{client: client}
}

func (s engineSpanSink) StartRunSpan(ctx context.Context, id engine.RunSpanID, at time.Time) (context.Context, engine.SynthSpan, error) {
	ctx, span, err := s.client.StartRun(ctx, telemetry.RunAttributes{
		StartedAt:       at,
		Gaggle:          id.Gaggle,
		WorkflowID:      id.WorkflowID,
		WorkflowVersion: id.WorkflowVersion,
		WorkflowDigest:  id.WorkflowDigest,
		RunID:           id.RunID,
	})
	if err != nil {
		return ctx, nil, err
	}
	return ctx, synthSpan{span}, nil
}

func (s engineSpanSink) StartStageSpan(ctx context.Context, id engine.StageSpanID, at time.Time) (context.Context, engine.SynthSpan, error) {
	ctx, span, err := s.client.StartTask(ctx, telemetry.TaskAttributes{
		StartedAt:       at,
		Gaggle:          id.Gaggle,
		WorkflowID:      id.WorkflowID,
		WorkflowVersion: id.WorkflowVersion,
		WorkflowDigest:  id.WorkflowDigest,
		RunID:           id.RunID,
		TaskID:          id.Stage,
		Attempt:         id.Attempt,
		Branch:          id.Branch,
	})
	if err != nil {
		return ctx, nil, err
	}
	return ctx, synthSpan{span}, nil
}

func (s engineSpanSink) StartGateSpan(ctx context.Context, id engine.GateSpanID, at time.Time) (context.Context, engine.SynthSpan, error) {
	ctx, span, err := s.client.StartGate(ctx, telemetry.GateAttributes{
		StartedAt:       at,
		Gaggle:          id.Gaggle,
		WorkflowID:      id.WorkflowID,
		WorkflowVersion: id.WorkflowVersion,
		WorkflowDigest:  id.WorkflowDigest,
		RunID:           id.RunID,
		GateID:          id.Gate,
		Decision:        id.Decision,
	})
	if err != nil {
		return ctx, nil, err
	}
	return ctx, synthSpan{span}, nil
}

// synthSpan narrows telemetry.Span to the two operations synthesis performs.
type synthSpan struct{ span telemetry.Span }

func (s synthSpan) Complete(outcome string, isFailure bool) { s.span.Complete(outcome, isFailure) }
func (s synthSpan) EndAt(at time.Time)                      { s.span.EndAt(at) }
