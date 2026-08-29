package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

// SpanSink is the narrow slice of a telemetry client this package needs to
// turn a completed run's projection into spans.
//
// Declared here rather than importing internal/telemetry so the engine keeps no
// dependency on the telemetry implementation: the daemon adapts its own client
// to this interface at the wiring site. It is also what makes the synthesis
// testable without an exporter, an SDK, or a clock.
type SpanSink interface {
	// StartRunSpan opens the root span for a run, backdated to at.
	StartRunSpan(ctx context.Context, id RunSpanID, at time.Time) (context.Context, SynthSpan, error)
	// StartStageSpan opens a stage-attempt span under the run's context.
	StartStageSpan(ctx context.Context, id StageSpanID, at time.Time) (context.Context, SynthSpan, error)
	// StartGateSpan opens a gate-evaluation span under the run's context.
	StartGateSpan(ctx context.Context, id GateSpanID, at time.Time) (context.Context, SynthSpan, error)
}

// SynthSpan is a span the synthesizer can grade and close at an explicit time.
type SynthSpan interface {
	// Complete grades the span. outcome is the journal status verbatim.
	Complete(outcome string, isFailure bool)
	// EndAt closes the span at the recorded time rather than now.
	EndAt(at time.Time)
}

// RunSpanID identifies the run a root span describes.
type RunSpanID struct {
	Gaggle          string
	WorkflowID      string
	WorkflowVersion string
	WorkflowDigest  string
	RunID           string
}

// StageSpanID identifies one stage attempt.
type StageSpanID struct {
	RunSpanID
	Stage   string
	Attempt int
	Branch  int
}

// GateSpanID identifies one gate evaluation.
type GateSpanID struct {
	RunSpanID
	Gate     string
	Decision string
}

// SynthesizeRunSpans turns a completed run's journal projection into spans.
//
// WHY THIS EXISTS AT ALL. The tier-3 engine emits no telemetry, so an engine run
// executes correctly and then appears in no dashboard built on tier-1 spans
// (#2865) — observed directly: three engine runs in a cluster with a working
// Application Insights pipeline produced zero rows.
//
// WHY SYNTHESIS RATHER THAN LIVE EMISSION. The projection is already a total
// function of workflow history (#629), and it carries everything a span needs:
// stage boundaries, attempts, gate decisions, outcomes, and the
// workflow-deterministic time of every write. Emitting live would instead put an
// exporter on the workflow and activity path, where a side effect inside
// deterministic workflow code is a thing to be careful with. The cost is that
// telemetry arrives when the run CLOSES rather than while it runs; that is a
// real tradeoff and the right one for a first cut.
//
// EVERY SPAN IS BACKDATED. Ops carry the time the decision was made, so a
// synthesized span reports the run's real moment and duration rather than the
// projection's. A span stamped at synthesis time would be worse than no span:
// it looks authoritative and is wrong.
//
// CORRELATION COMES FREE. The sink derives the trace id from the run id, and
// engine/starter.go already sets the Temporal workflow id to the run id, so one
// identifier joins the trace backend, the journal, and the Temporal UI.
//
// A nil sink is a no-op, which is the tier-1 case and an instance with
// telemetry disabled.
func SynthesizeRunSpans(ctx context.Context, sink SpanSink, proj JournalProjection) error {
	if sink == nil || len(proj.Ops) == 0 {
		return nil
	}
	run := RunSpanID{
		Gaggle:          proj.Identity.Gaggle,
		WorkflowID:      proj.Identity.Workflow,
		WorkflowVersion: fmt.Sprintf("%d", proj.Identity.WorkflowVersion),
		WorkflowDigest:  proj.Identity.WorkflowDigest,
		RunID:           proj.Identity.RunID,
	}
	if run.RunID == "" || run.Gaggle == "" || run.WorkflowID == "" {
		return fmt.Errorf("engine: cannot synthesize spans without run identity")
	}

	first, last := proj.Ops[0], proj.Ops[len(proj.Ops)-1]
	runCtx, runSpan, err := sink.StartRunSpan(ctx, run, first.Time)
	if err != nil {
		return fmt.Errorf("engine: synthesize run span: %w", err)
	}

	// Stage spans are opened on stage.started and closed on the matching
	// stage.finished. Keyed by stage AND attempt because a retried stage is a
	// new attempt with its own span, exactly as the local runner records it —
	// collapsing them would hide the retry that is usually the interesting part.
	open := map[stageKey]SynthSpan{}
	for _, op := range proj.Ops {
		ev := op.Event
		if op.Kind != opAppend || ev == nil {
			continue
		}
		switch ev.Type {
		case journal.EventStageStarted:
			key := stageKey{stage: ev.Stage, attempt: ev.Attempt}
			_, span, err := sink.StartStageSpan(runCtx, StageSpanID{
				RunSpanID: run, Stage: ev.Stage, Attempt: ev.Attempt, Branch: ev.Branch,
			}, op.Time)
			if err != nil {
				return fmt.Errorf("engine: synthesize stage span %q: %w", ev.Stage, err)
			}
			open[key] = span
		case journal.EventStageFinished:
			key := stageKey{stage: ev.Stage, attempt: ev.Attempt}
			span, ok := open[key]
			if !ok {
				// A finish with no start is a malformed projection, not
				// something to paper over with a zero-length span.
				return fmt.Errorf("engine: stage %q attempt %d finished without a start", ev.Stage, ev.Attempt)
			}
			span.Complete(ev.Status, isFailureStatus(ev.Status))
			span.EndAt(op.Time)
			delete(open, key)
		case journal.EventGateEvaluated:
			// A gate evaluation is a point decision in the projection: there is
			// no paired start, so the span spans the instant it was recorded.
			_, span, err := sink.StartGateSpan(runCtx, GateSpanID{
				RunSpanID: run, Gate: ev.Gate, Decision: ev.Verdict,
			}, op.Time)
			if err != nil {
				return fmt.Errorf("engine: synthesize gate span %q: %w", ev.Gate, err)
			}
			span.Complete(ev.Verdict, false)
			span.EndAt(op.Time)
		}
	}

	// A stage still open at run end never recorded a finish. Close it at the
	// run's end rather than leaking an unterminated span, and grade it as the
	// incomplete thing it is.
	for key, span := range open {
		span.Complete("incomplete", true)
		span.EndAt(last.Time)
		_ = key
	}

	status := ""
	if last.Event != nil {
		status = last.Event.Status
	}
	runSpan.Complete(status, isFailureStatus(status))
	runSpan.EndAt(last.Time)
	return nil
}

type stageKey struct {
	stage   string
	attempt int
}

// isFailureStatus maps a journal status to the span's failure axis. Only
// terminal-bad statuses are failures: "blocked" and "escalated" are outcomes a
// run is designed to produce, and grading them as failures would make every
// dashboard's error rate meaningless.
func isFailureStatus(status string) bool {
	switch status {
	case string(journal.PhaseFailed), "error":
		return true
	default:
		return false
	}
}
