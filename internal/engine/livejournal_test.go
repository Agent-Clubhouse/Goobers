package engine

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/testsuite"

	"github.com/goobers/goobers/internal/runsemantics"

	// livejournal_test.go covers the engine side of the live journal service
	// (distributed-state-and-coordination.md §8, DS4/DS5) against §13's
	// acceptance items 3 and 4:
	//
	//   - a run with LiveJournal pinned authors its journal mid-flight through
	//     the emission seam — stage transitions are on disk BEFORE the run
	//     closes (item 3's visibility half; the StalledRunTimeout half lives
	//     with the sweep in cmd/goobers);
	//   - a redelivered emission (retried activity) is deduplicated: exactly one
	//     copy per idempotency key, and the live journal's normative view diffs
	//     empty against an independent history re-projection (item 4);
	//   - an emission that exhausts its bounded budget fails the attempt as
	//     attemptClass infra, never the policy budget (§8 failure policy, #3361);
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/temporaltest"
	wf "github.com/goobers/goobers/internal/workflow"
)

// newLiveWriter builds a real daemon live writer over a temp runs directory —
// the in-process emission seam, exactly as the daemon wires it.

// executeLive runs one engine fixture with a wired journal emitter and
// returns the queried projection (the deterministic repair/cross-check
// source, which REMAINS accumulated for live runs).
func executeLive(t *testing.T, in RunInput, acts *Activities, wantWorkflowErr bool) JournalProjection {
	t.Helper()
	var ts testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&ts)
	env.SetStartTime(time.Date(2026, 8, 22, 3, 4, 5, 0, time.UTC))
	env.RegisterActivity(acts)
	env.ExecuteWorkflow(Run, in)
	if err := env.GetWorkflowError(); (err != nil) != wantWorkflowErr {
		t.Fatalf("workflow error = %v, wantWorkflowErr = %t", err, wantWorkflowErr)
	}
	if wantWorkflowErr {
		if err := env.GetWorkflowError(); !strings.Contains(err.Error(), "journal stage") {
			t.Fatalf("workflow error = %v, want the journal-stage failure", err)
		}
	}
	val, err := env.QueryWorkflow(JournalQuery)
	if err != nil {
		t.Fatalf("query projection: %v", err)
	}
	var proj JournalProjection
	if err := val.Get(&proj); err != nil {
		t.Fatalf("decode projection: %v", err)
	}
	return proj
}

// journalPeekRunner is a deterministic stage executor that inspects the live
// journal WHILE the stage is executing — the mid-flight observer §13 item 3
// requires. It records what the journal showed at dispatch time.
type journalPeekRunner struct {
	dir string

	mu              sync.Mutex
	peeked          bool
	sawStageStarted bool
	sawTerminal     bool
	phase           journal.RunPhase
	peekErr         error
}

func (p *journalPeekRunner) Run(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.peeked = true
	rd, err := journal.OpenRead(p.dir)
	if err != nil {
		p.peekErr = err
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
	}
	events, err := rd.Events()
	if err != nil {
		p.peekErr = err
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
	}
	for _, ev := range events {
		if ev.Type == journal.EventStageStarted && ev.Stage == "implement" {
			p.sawStageStarted = true
		}
		if ev.Type == journal.EventRunFinished {
			p.sawTerminal = true
		}
	}
	p.phase, p.peekErr = rd.Phase()
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}

// TestLiveJournalShowsStageTransitionMidRun is §13 item 3's visibility half:
// with live authorship (DS4) the run journal exists — and shows the stage
// transition — while the stage is still executing, not minutes after close.
// The terminal then lands through the same seam, and the journal's normative
// view diffs empty against an independent history re-projection (DS5's
// cross-check, the A2 comparison surface).
func TestLiveJournalShowsStageTransitionMidRun(t *testing.T) {
	writer, runsDir := newLiveWriter(t)
	spec := crSpec("implement",
		[]apiv1.Task{crTask("implement", "review")},
		[]apiv1.Gate{crGate("review", map[string]string{"pass": wf.TerminalComplete, "fail": wf.TargetAbort})})
	in := projectionInput("live-mid", spec)
	in.LiveJournal = true
	peek := &journalPeekRunner{dir: filepath.Join(runsDir, in.RunID)}

	proj := executeLive(t, in, &Activities{
		Det:        peek,
		Auto:       gate.NewAutomatedEvaluator(),
		Workspaces: testWorkspaces(t),
		Journal:    writer,
	}, false)

	peek.mu.Lock()
	defer peek.mu.Unlock()
	if !peek.peeked {
		t.Fatal("stage executor never ran")
	}
	if peek.peekErr != nil {
		t.Fatalf("mid-run journal peek: %v", peek.peekErr)
	}
	if !peek.sawStageStarted {
		t.Fatal("stage.started was not in the live journal while the stage executed")
	}
	if peek.sawTerminal {
		t.Fatal("journal was already terminal during stage execution")
	}
	if peek.phase != journal.PhaseRunning {
		t.Fatalf("mid-run phase = %q, want running", peek.phase)
	}

	events := liveEvents(t, runsDir, in.RunID)
	if err := journal.MonotonicSeq(events); err != nil {
		t.Fatal(err)
	}
	last := events[len(events)-1]
	if last.Type != journal.EventRunFinished || last.Status != string(journal.PhaseCompleted) {
		t.Fatalf("live journal ends with %+v, want run.finished completed", last)
	}
	if writer.IsOpen(in.RunID) {
		t.Fatal("writer still holds the terminal run open")
	}
	divergence, err := diffTestJournal(t, events, proj)
	if err != nil {
		t.Fatalf("compare journal: %v", err)
	}
	if divergence != "" {
		t.Fatalf("live journal diverges from history re-projection:\n%s", divergence)
	}
}

// ackLossEmitter delivers each batch to the real writer, then drops exactly
// one success response — the retried-activity shape §13 item 4 names: the
// write was durable, the ack was lost, and Temporal re-runs the activity,
// which re-emits the same batch under the same idempotency keys.
type ackLossEmitter struct {
	writer *livejournal.Writer

	mu      sync.Mutex
	dropped bool
}

func (e *ackLossEmitter) Emit(ctx context.Context, req livejournal.EmitRequest) (livejournal.EmitResponse, error) {
	resp, err := e.writer.Emit(ctx, req)
	if err != nil {
		return resp, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.dropped {
		e.dropped = true
		return livejournal.EmitResponse{}, errors.New("simulated ack loss after the durable write")
	}
	return resp, nil
}

// TestLiveJournalRedeliveryIsDeduplicated is §13 item 4: a forcibly-retried
// emission re-delivers its events, the journal holds exactly one copy per
// idempotency key with the originally-allocated seq, and the conformance
// diff against the history re-projection is empty.
func TestLiveJournalRedeliveryIsDeduplicated(t *testing.T) {
	writer, runsDir := newLiveWriter(t)
	emitter := &ackLossEmitter{writer: writer}
	in := projectionInput("live-redeliver", crSpec("implement", []apiv1.Task{crTask("implement", "")}, nil))
	in.LiveJournal = true

	proj := executeLive(t, in, &Activities{
		Det:        &scriptedStages{},
		Workspaces: testWorkspaces(t),
		Journal:    emitter,
	}, false)

	emitter.mu.Lock()
	dropped := emitter.dropped
	emitter.mu.Unlock()
	if !dropped {
		t.Fatal("fixture never exercised the redelivery path")
	}

	events := liveEvents(t, runsDir, in.RunID)
	if err := journal.MonotonicSeq(events); err != nil {
		t.Fatal(err)
	}
	seen := map[string]uint64{}
	for _, ev := range events {
		key, _ := ev.Runner[livejournal.EmitKeyRunnerField].(string)
		if key == "" {
			continue
		}
		if prior, dup := seen[key]; dup {
			t.Fatalf("idempotency key %q appears at seq %d and %d — redelivery duplicated an event", key, prior, ev.Seq)
		}
		seen[key] = ev.Seq
	}
	if len(events) != len(proj.Ops) {
		t.Fatalf("live journal has %d events, projection accumulated %d ops", len(events), len(proj.Ops))
	}
	divergence, err := diffTestJournal(t, events, proj)
	if err != nil {
		t.Fatalf("compare journal: %v", err)
	}
	if divergence != "" {
		t.Fatalf("redelivered run diverges from history re-projection:\n%s", divergence)
	}
}

// failAfterOpenEmitter lets the opening batch through, then refuses every
// emission — the unreachable-daemon shape of §8's failure policy.
type failAfterOpenEmitter struct {
	writer *livejournal.Writer

	mu    sync.Mutex
	calls int
}

func (e *failAfterOpenEmitter) Emit(ctx context.Context, req livejournal.EmitRequest) (livejournal.EmitResponse, error) {
	e.mu.Lock()
	e.calls++
	call := e.calls
	e.mu.Unlock()
	if call > 1 {
		return livejournal.EmitResponse{}, errors.New("journal plane unreachable")
	}
	return e.writer.Emit(ctx, req)
}

// TestLiveJournalEmitExhaustionFailsAttemptAsInfra pins §8's failure policy
// (#3361): a stage whose journal emission exhausts its bounded budget fails
// as attemptClass infra — its rolled-back ops never reach the projection, the
// infra budget (not the policy budget) is consumed, and the on-disk journal
// is left non-terminal at its last durable boundary (the wedged shape the
// stalled-run sweep and the DS5 backfill then handle).
func TestLiveJournalEmitExhaustionFailsAttemptAsInfra(t *testing.T) {
	writer, runsDir := newLiveWriter(t)
	emitter := &failAfterOpenEmitter{writer: writer}
	in := projectionInput("live-unreachable", crSpec("implement", []apiv1.Task{crTask("implement", "")}, nil))
	in.LiveJournal = true

	proj := executeLive(t, in, &Activities{
		Det:        &scriptedStages{},
		Workspaces: testWorkspaces(t),
		Journal:    emitter,
	}, true)

	var emitFailures int
	for _, op := range proj.Ops {
		if op.Event == nil {
			continue
		}
		switch op.Event.Type {
		case journal.EventStageStarted, journal.EventStageFinished:
			t.Fatalf("rolled-back stage op survived in the projection: %+v", op.Event)
		case journal.EventError:
			if op.Event.Error != nil && op.Event.Error.Code == JournalEmitFailedErrorCode {
				if op.Event.AttemptClass != journal.AttemptInfra {
					t.Fatalf("emit failure classed %q, want infra", op.Event.AttemptClass)
				}
				emitFailures++
			}
		}
	}
	if emitFailures != int(runsemantics.DefaultMaxInfrastructureAttempts) {
		t.Fatalf("journal_emit_failed events = %d, want the infra budget %d", emitFailures, runsemantics.DefaultMaxInfrastructureAttempts)
	}
	terminal := proj.Ops[len(proj.Ops)-1].Event
	if terminal == nil || terminal.Type != journal.EventRunFinished || terminal.Status != string(journal.PhaseFailed) {
		t.Fatalf("projection terminal = %+v, want run.finished failed", terminal)
	}

	cause := terminal.TerminalCause
	if cause == nil || cause.Classification != journal.TerminalInfrastructureFailure ||
		cause.SelectorKind != "stage" || cause.Selector != "implement" || cause.Retry == nil ||
		cause.Retry.Consumed != int(runsemantics.DefaultMaxInfrastructureAttempts)-1 ||
		cause.Retry.Allowed != int(runsemantics.DefaultMaxInfrastructureAttempts)-1 ||
		cause.CausalEventSeq == 0 || cause.CausalEmitKey == "" {
		t.Fatalf("journal emission exhaustion cause = %+v", cause)
	}

	// On disk: only the opening emission landed; the run is wedged
	// non-terminal — exactly what StalledRunTimeout keys off and what the
	// demoted reconciler backfills.
	events := liveEvents(t, runsDir, in.RunID)
	if len(events) != 1 || events[0].Type != journal.EventRunStarted {
		t.Fatalf("wedged journal events = %+v, want only run.started", events)
	}
	rd, err := journal.OpenRead(filepath.Join(runsDir, in.RunID))
	if err != nil {
		t.Fatal(err)
	}
	phase, err := rd.Phase()
	if err != nil {
		t.Fatal(err)
	}
	if phase != journal.PhaseRunning {
		t.Fatalf("wedged journal phase = %q, want running", phase)
	}
}
