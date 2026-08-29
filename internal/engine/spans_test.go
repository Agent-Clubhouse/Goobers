package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

type recordedSpan struct {
	kind     string
	name     string
	attempt  int
	start    time.Time
	end      time.Time
	outcome  string
	failure  bool
	finished bool
}

type fakeSink struct {
	spans   []*recordedSpan
	failOn  string
	runCtxs int
}

func (f *fakeSink) record(kind, name string, attempt int, at time.Time) *recordedSpan {
	s := &recordedSpan{kind: kind, name: name, attempt: attempt, start: at}
	f.spans = append(f.spans, s)
	return s
}

func (f *fakeSink) StartRunSpan(ctx context.Context, id RunSpanID, at time.Time) (context.Context, SynthSpan, error) {
	f.runCtxs++
	return ctx, f.record("run", id.WorkflowID, 0, at), nil
}

func (f *fakeSink) StartStageSpan(ctx context.Context, id StageSpanID, at time.Time) (context.Context, SynthSpan, error) {
	if f.failOn == id.Stage {
		return ctx, nil, context.Canceled
	}
	return ctx, f.record("stage", id.Stage, id.Attempt, at), nil
}

func (f *fakeSink) StartGateSpan(ctx context.Context, id GateSpanID, at time.Time) (context.Context, SynthSpan, error) {
	return ctx, f.record("gate", id.Gate, 0, at), nil
}

func (s *recordedSpan) Complete(outcome string, isFailure bool) {
	s.outcome, s.failure = outcome, isFailure
}
func (s *recordedSpan) EndAt(at time.Time) { s.end, s.finished = at, true }

func at(sec int) time.Time {
	return time.Date(2026, 8, 12, 11, 42, 0, 0, time.UTC).Add(time.Duration(sec) * time.Second)
}

func appendOp(t journal.EventType, sec int, mutate func(*journal.Event)) JournalOp {
	ev := &journal.Event{Type: t, Time: at(sec)}
	if mutate != nil {
		mutate(ev)
	}
	return JournalOp{Kind: opAppend, Event: ev, Time: at(sec)}
}

// The shape of the real cross-platform run that motivated this: claim on Linux,
// agentic stage on Windows, gate, merge — 116 seconds end to end.
func crossPlatformProjection() JournalProjection {
	return JournalProjection{
		Identity: journal.RunIdentity{
			RunID: "9740bc5d00465885af6fa5b1b263c61e", Gaggle: "testbed-minimal",
			Workflow: "cross-platform-implement", WorkflowVersion: 1,
		},
		Ops: []JournalOp{
			appendOp(journal.EventRunStarted, 0, func(e *journal.Event) { e.Status = "running" }),
			appendOp(journal.EventStageStarted, 1, func(e *journal.Event) { e.Stage = "query-backlog"; e.Attempt = 1 }),
			appendOp(journal.EventStageFinished, 8, func(e *journal.Event) {
				e.Stage, e.Attempt, e.Status = "query-backlog", 1, "success"
			}),
			appendOp(journal.EventStageStarted, 8, func(e *journal.Event) { e.Stage = "implement"; e.Attempt = 1 }),
			appendOp(journal.EventStageFinished, 82, func(e *journal.Event) {
				e.Stage, e.Attempt, e.Status = "implement", 1, "success"
			}),
			appendOp(journal.EventGateEvaluated, 110, func(e *journal.Event) { e.Gate = "ci-gate"; e.Verdict = "pass" }),
			appendOp(journal.EventRunFinished, 116, func(e *journal.Event) { e.Status = "completed" }),
		},
	}
}

func TestSynthesizeRunSpansBackdatesTheWholeRun(t *testing.T) {
	t.Parallel()
	sink := &fakeSink{}
	if err := SynthesizeRunSpans(context.Background(), sink, crossPlatformProjection()); err != nil {
		t.Fatalf("SynthesizeRunSpans: %v", err)
	}

	if got := len(sink.spans); got != 4 {
		t.Fatalf("synthesized %d spans, want 4 (run + 2 stages + gate)", got)
	}
	run := sink.spans[0]
	if run.kind != "run" {
		t.Fatalf("first span is %q, want the run root", run.kind)
	}
	// The run's real duration, not the projection's.
	if d := run.end.Sub(run.start); d != 116*time.Second {
		t.Fatalf("run duration = %s, want 1m56s — the span must carry the run's time, not synthesis time", d)
	}
	if run.outcome != "completed" || run.failure {
		t.Fatalf("run graded %q failure=%v, want completed/false", run.outcome, run.failure)
	}
	for _, s := range sink.spans {
		if !s.finished {
			t.Fatalf("%s span %q was never closed — a leaked span is worse than no span", s.kind, s.name)
		}
	}
}

func TestSynthesizeRunSpansGivesEachAttemptItsOwnSpan(t *testing.T) {
	t.Parallel()
	proj := JournalProjection{
		Identity: journal.RunIdentity{RunID: "r", Gaggle: "g", Workflow: "w", WorkflowVersion: 1},
		Ops: []JournalOp{
			appendOp(journal.EventRunStarted, 0, nil),
			appendOp(journal.EventStageStarted, 1, func(e *journal.Event) { e.Stage = "flaky"; e.Attempt = 1 }),
			appendOp(journal.EventStageFinished, 5, func(e *journal.Event) {
				e.Stage, e.Attempt, e.Status = "flaky", 1, "failed"
			}),
			appendOp(journal.EventStageStarted, 6, func(e *journal.Event) { e.Stage = "flaky"; e.Attempt = 2 }),
			appendOp(journal.EventStageFinished, 9, func(e *journal.Event) {
				e.Stage, e.Attempt, e.Status = "flaky", 2, "success"
			}),
			appendOp(journal.EventRunFinished, 10, func(e *journal.Event) { e.Status = "completed" }),
		},
	}
	sink := &fakeSink{}
	if err := SynthesizeRunSpans(context.Background(), sink, proj); err != nil {
		t.Fatalf("SynthesizeRunSpans: %v", err)
	}
	var attempts []int
	for _, s := range sink.spans {
		if s.kind == "stage" {
			attempts = append(attempts, s.attempt)
		}
	}
	if len(attempts) != 2 || attempts[0] != 1 || attempts[1] != 2 {
		t.Fatalf("stage attempts = %v, want [1 2] — collapsing retries hides the interesting part", attempts)
	}
	if !sink.spans[1].failure {
		t.Fatal("attempt 1 finished 'failed' and must be graded a failure")
	}
	if sink.spans[2].failure {
		t.Fatal("attempt 2 finished 'success' and must not be graded a failure")
	}
}

// blocked and escalated are outcomes a run is designed to produce. Grading them
// as failures would make every dashboard's error rate meaningless.
func TestSynthesizeRunSpansDoesNotGradeBlockedAsFailure(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"blocked", "escalated", "completed"} {
		proj := JournalProjection{
			Identity: journal.RunIdentity{RunID: "r", Gaggle: "g", Workflow: "w", WorkflowVersion: 1},
			Ops: []JournalOp{
				appendOp(journal.EventRunStarted, 0, nil),
				appendOp(journal.EventRunFinished, 4, func(e *journal.Event) { e.Status = status }),
			},
		}
		sink := &fakeSink{}
		if err := SynthesizeRunSpans(context.Background(), sink, proj); err != nil {
			t.Fatalf("%s: %v", status, err)
		}
		if sink.spans[0].failure {
			t.Fatalf("status %q graded as a failure; only terminal-bad statuses are failures", status)
		}
	}
}

// A stage that never recorded a finish must not leak an unterminated span.
func TestSynthesizeRunSpansClosesStagesLeftOpen(t *testing.T) {
	t.Parallel()
	proj := JournalProjection{
		Identity: journal.RunIdentity{RunID: "r", Gaggle: "g", Workflow: "w", WorkflowVersion: 1},
		Ops: []JournalOp{
			appendOp(journal.EventRunStarted, 0, nil),
			appendOp(journal.EventStageStarted, 2, func(e *journal.Event) { e.Stage = "abandoned"; e.Attempt = 1 }),
			appendOp(journal.EventRunFinished, 30, func(e *journal.Event) { e.Status = "failed" }),
		},
	}
	sink := &fakeSink{}
	if err := SynthesizeRunSpans(context.Background(), sink, proj); err != nil {
		t.Fatalf("SynthesizeRunSpans: %v", err)
	}
	stage := sink.spans[1]
	if !stage.finished {
		t.Fatal("a stage left open at run end must still be closed")
	}
	if !stage.failure || stage.outcome != "incomplete" {
		t.Fatalf("abandoned stage graded %q failure=%v, want incomplete/true", stage.outcome, stage.failure)
	}
	if !stage.end.Equal(at(30)) {
		t.Fatalf("abandoned stage ended %s, want the run's end %s", stage.end, at(30))
	}
}

// A finish with no start means the projection is malformed. Papering over it
// with a zero-length span would publish a lie.
func TestSynthesizeRunSpansRejectsFinishWithoutStart(t *testing.T) {
	t.Parallel()
	proj := JournalProjection{
		Identity: journal.RunIdentity{RunID: "r", Gaggle: "g", Workflow: "w", WorkflowVersion: 1},
		Ops: []JournalOp{
			appendOp(journal.EventRunStarted, 0, nil),
			appendOp(journal.EventStageFinished, 3, func(e *journal.Event) {
				e.Stage, e.Attempt, e.Status = "ghost", 1, "success"
			}),
			appendOp(journal.EventRunFinished, 4, func(e *journal.Event) { e.Status = "completed" }),
		},
	}
	err := SynthesizeRunSpans(context.Background(), &fakeSink{}, proj)
	if err == nil || !strings.Contains(err.Error(), "without a start") {
		t.Fatalf("err = %v, want a rejection naming the unmatched finish", err)
	}
}

// tier 1, and any instance with telemetry disabled.
func TestSynthesizeRunSpansNilSinkIsNoOp(t *testing.T) {
	t.Parallel()
	if err := SynthesizeRunSpans(context.Background(), nil, crossPlatformProjection()); err != nil {
		t.Fatalf("nil sink must be a no-op, got %v", err)
	}
}

func TestSynthesizeRunSpansRequiresIdentity(t *testing.T) {
	t.Parallel()
	proj := JournalProjection{Ops: []JournalOp{appendOp(journal.EventRunStarted, 0, nil)}}
	if err := SynthesizeRunSpans(context.Background(), &fakeSink{}, proj); err == nil {
		t.Fatal("a projection with no run identity must be refused, not emitted under an empty trace")
	}
}
