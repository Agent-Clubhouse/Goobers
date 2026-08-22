package engine

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
//   - the demoted reconciler (DS5) VERIFIES a live-authored journal instead
//     of rewriting it, files divergences to the named channel, skips runs
//     the writer still holds open, and backfills — visibly — the
//     crash-orphan case.

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	commonpb "go.temporal.io/api/common/v1"
	workflowpb "go.temporal.io/api/workflow/v1"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/testsuite"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/temporaltest"
	wf "github.com/goobers/goobers/internal/workflow"
)

// newLiveWriter builds a real daemon live writer over a temp runs directory —
// the in-process emission seam, exactly as the daemon wires it.
func newLiveWriter(t *testing.T) (*livejournal.Writer, string) {
	t.Helper()
	runsDir := filepath.Join(t.TempDir(), "runs")
	w, err := livejournal.NewWriter(func(gaggle string) (string, bool) {
		if gaggle != "web" {
			return "", false
		}
		return runsDir, true
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Close)
	return w, runsDir
}

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

func liveEvents(t *testing.T, runsDir, runID string) []journal.Event {
	t.Helper()
	rd, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatal(err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatal(err)
	}
	return events
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
	if !livejournal.Authored(events) {
		t.Fatal("live journal does not read as live-authored")
	}
	if writer.IsOpen(in.RunID) {
		t.Fatal("writer still holds the terminal run open")
	}
	divergence, err := DiffLiveJournal(events, proj)
	if err != nil {
		t.Fatalf("DiffLiveJournal: %v", err)
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
	divergence, err := DiffLiveJournal(events, proj)
	if err != nil {
		t.Fatalf("DiffLiveJournal: %v", err)
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
	if emitFailures != int(runner.DefaultMaxInfrastructureAttempts) {
		t.Fatalf("journal_emit_failed events = %d, want the infra budget %d", emitFailures, runner.DefaultMaxInfrastructureAttempts)
	}
	terminal := proj.Ops[len(proj.Ops)-1].Event
	if terminal == nil || terminal.Type != journal.EventRunFinished || terminal.Status != string(journal.PhaseFailed) {
		t.Fatalf("projection terminal = %+v, want run.finished failed", terminal)
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

// --- DS5: the demoted reconciler ---

// liveAuthor replays a projection's ops through a real live writer, as the
// engine's emission would, so reconciler tests get a byte-real live-authored
// journal without running a second workflow.
func liveAuthor(t *testing.T, w *livejournal.Writer, proj JournalProjection, count int) {
	t.Helper()
	ops := make([]livejournal.Op, 0, count)
	for i, op := range proj.Ops[:count] {
		lop := liveOpFrom(op)
		lop.Key = fmt.Sprintf("%s|%d", proj.Identity.RunID, i)
		ops = append(ops, lop)
	}
	_, err := w.Emit(context.Background(), livejournal.EmitRequest{
		RunID:  proj.Identity.RunID,
		Gaggle: proj.Identity.Gaggle,
		Open: &livejournal.OpenHeader{
			Identity:   proj.Identity,
			Item:       proj.Item,
			Graph:      proj.Graph,
			Definition: proj.Definition,
		},
		Ops: ops,
	})
	if err != nil {
		t.Fatalf("live-author fixture journal: %v", err)
	}
}

func reconcilerFor(t *testing.T, proj JournalProjection, runsDir string, live LiveJournalRegistry, divergences *[]string) (*CompletedRunReconciler, *completedRunFake, *int) {
	t.Helper()
	payload, err := converter.GetDefaultDataConverter().ToPayload(proj.Identity.Gaggle)
	if err != nil {
		t.Fatal(err)
	}
	fake := &completedRunFake{
		projection: proj,
		executions: []*workflowpb.WorkflowExecutionInfo{{
			Execution: &commonpb.WorkflowExecution{WorkflowId: proj.Identity.RunID},
			Memo:      &commonpb.Memo{Fields: map[string]*commonpb.Payload{RunGaggleMemoKey: payload}},
		}},
	}
	observations := 0
	reconciler, err := NewCompletedRunReconciler(fake, "default", map[string]string{proj.Identity.Gaggle: runsDir}, func(context.Context, string, uint64) error {
		observations++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	reconciler = reconciler.WithLiveJournals(live).WithDivergenceReporter(func(runID, detail string) {
		*divergences = append(*divergences, runID+": "+detail)
	})
	return reconciler, fake, &observations
}

// TestReconcilerVerifiesLiveAuthoredJournalInsteadOfRewriting is DS5's core
// demotion: a complete live-authored journal is diffed against the history
// re-projection, never rewritten — the journal on disk keeps its live-author
// marks, agreement files nothing, and the verification runs once, not every
// cycle.
func TestReconcilerVerifiesLiveAuthoredJournalInsteadOfRewriting(t *testing.T) {
	proj := executeForProjection(t, projectionInput("live-verify", crSpec("implement", []apiv1.Task{crTask("implement", "")}, nil)), &Activities{
		Det:        &scriptedStages{},
		Workspaces: testWorkspaces(t),
	}, false)
	writer, runsDir := newLiveWriter(t)
	liveAuthor(t, writer, proj, len(proj.Ops))

	var divergences []string
	reconciler, fake, observations := reconcilerFor(t, proj, runsDir, writer, &divergences)
	if count, err := reconciler.Reconcile(context.Background()); err != nil || count != 0 {
		t.Fatalf("Reconcile = (%d, %v), want (0, nil): verification is not a projection", count, err)
	}
	if len(divergences) != 0 {
		t.Fatalf("agreeing journal filed divergences: %v", divergences)
	}
	if fake.queries != 1 {
		t.Fatalf("projection queries = %d, want exactly the verification query", fake.queries)
	}
	if *observations != 1 {
		t.Fatalf("observer calls = %d, want 1", *observations)
	}
	events := liveEvents(t, runsDir, proj.Identity.RunID)
	if !livejournal.Authored(events) {
		t.Fatal("verification overwrote the live-authored journal (emit keys gone)")
	}

	// The second cycle re-observes but does not re-verify.
	if count, err := reconciler.Reconcile(context.Background()); err != nil || count != 0 {
		t.Fatalf("second Reconcile = (%d, %v)", count, err)
	}
	if fake.queries != 1 {
		t.Fatalf("verified run was re-queried: queries = %d", fake.queries)
	}
}

// TestReconcilerFilesDivergenceToTheNamedChannel: a live journal whose
// normative view disagrees with the history re-projection is REPORTED — to
// the channel the #2871 parity ledger reads — and left exactly as authored,
// never silently repaired.
func TestReconcilerFilesDivergenceToTheNamedChannel(t *testing.T) {
	proj := executeForProjection(t, projectionInput("live-diverge", crSpec("implement", []apiv1.Task{crTask("implement", "")}, nil)), &Activities{
		Det:        &scriptedStages{},
		Workspaces: testWorkspaces(t),
	}, false)

	// Author the live journal with one normative event mutated: the stage
	// outcome flipped, the shape a real authorship bug would take.
	mutated := proj
	mutated.Ops = append([]JournalOp(nil), proj.Ops...)
	found := false
	for i, op := range mutated.Ops {
		if op.Event != nil && op.Event.Type == journal.EventStageFinished {
			ev := *op.Event
			ev.Status = string(apiv1.ResultFailure)
			mutated.Ops[i].Event = &ev
			found = true
			break
		}
	}
	if !found {
		t.Fatal("fixture has no stage.finished op")
	}
	writer, runsDir := newLiveWriter(t)
	liveAuthor(t, writer, mutated, len(mutated.Ops))

	var divergences []string
	reconciler, _, _ := reconcilerFor(t, proj, runsDir, writer, &divergences)
	if count, err := reconciler.Reconcile(context.Background()); err != nil || count != 0 {
		t.Fatalf("Reconcile = (%d, %v), want (0, nil)", count, err)
	}
	if len(divergences) != 1 || !strings.Contains(divergences[0], "diverges") {
		t.Fatalf("divergences = %v, want one filed divergence", divergences)
	}

	// Never silently repaired: the journal still says what the live author
	// said.
	events := liveEvents(t, runsDir, proj.Identity.RunID)
	var status string
	for _, ev := range events {
		if ev.Type == journal.EventStageFinished {
			status = ev.Status
		}
	}
	if status != string(apiv1.ResultFailure) {
		t.Fatalf("stage.finished status = %q — the diverging journal was rewritten", status)
	}
}

// stubRegistry pins the registry's answer independent of a real writer: true
// behaves like a writer that holds every run open (reservations refused),
// false grants every reservation.
type stubRegistry bool

func (s stubRegistry) IsOpen(string) bool { return bool(s) }

func (s stubRegistry) Reserve(string) (func(), bool) {
	if bool(s) {
		return nil, false
	}
	return func() {}, true
}

// TestReconcilerSkipsRunsTheLiveWriterHoldsOpen: Temporal reporting a
// workflow closed does not mean the terminal emission has landed; a journal
// the writer holds open is untouchable this cycle.
func TestReconcilerSkipsRunsTheLiveWriterHoldsOpen(t *testing.T) {
	proj := executeForProjection(t, projectionInput("live-open", crSpec("implement", []apiv1.Task{crTask("implement", "")}, nil)), &Activities{
		Det:        &scriptedStages{},
		Workspaces: testWorkspaces(t),
	}, false)
	writer, runsDir := newLiveWriter(t)
	liveAuthor(t, writer, proj, len(proj.Ops)-1) // no terminal: the writer keeps the journal open
	if !writer.IsOpen(proj.Identity.RunID) {
		t.Fatal("fixture writer does not hold the run open")
	}

	var divergences []string
	reconciler, fake, observations := reconcilerFor(t, proj, runsDir, writer, &divergences)
	if count, err := reconciler.Reconcile(context.Background()); err != nil || count != 0 {
		t.Fatalf("Reconcile = (%d, %v), want the open run skipped", count, err)
	}
	if fake.queries != 0 || *observations != 0 || len(divergences) != 0 {
		t.Fatalf("open run was touched: queries=%d observations=%d divergences=%v", fake.queries, *observations, divergences)
	}
}

// TestProjectRunRefusesReplacingAJournalALiveWriterHolds is the #3529 major's
// filesystem half: a partial journal whose run-dir lock a live writer holds
// must never be deleted-and-replaced out from under it — removal detaches the
// writer's events log into an unlinked inode, so it keeps acknowledging
// appends no reader of the published directory can see. The replace path
// refuses with journal.ErrRunActive, and the writer's next acknowledged emit
// stays readable from the published run directory.
func TestProjectRunRefusesReplacingAJournalALiveWriterHolds(t *testing.T) {
	proj := executeForProjection(t, projectionInput("live-held", crSpec("implement", []apiv1.Task{crTask("implement", "")}, nil)), &Activities{
		Det:        &scriptedStages{},
		Workspaces: testWorkspaces(t),
	}, false)
	writer, runsDir := newLiveWriter(t)
	liveAuthor(t, writer, proj, len(proj.Ops)-1) // partial: the writer holds the journal open
	if !writer.IsOpen(proj.Identity.RunID) {
		t.Fatal("fixture writer does not hold the run open")
	}

	if _, err := ProjectRun(runsDir, proj); !errors.Is(err, journal.ErrRunActive) {
		t.Fatalf("ProjectRun err = %v, want journal.ErrRunActive", err)
	}

	stragglerKey := proj.Identity.RunID + "|straggler"
	resp, err := writer.Emit(context.Background(), livejournal.EmitRequest{
		RunID:  proj.Identity.RunID,
		Gaggle: proj.Identity.Gaggle,
		Ops: []livejournal.Op{{Kind: livejournal.OpAppend, Key: stragglerKey, Time: time.Date(2026, 8, 22, 4, 0, 0, 0, time.UTC), Event: &journal.Event{
			Type: journal.EventError, Error: &journal.ErrorDetail{Code: "straggler", Message: "late emission"},
		}}},
	})
	if err != nil || resp.Applied != 1 {
		t.Fatalf("straggler emit = (%+v, %v), want one applied op", resp, err)
	}
	published := false
	for _, ev := range liveEvents(t, runsDir, proj.Identity.RunID) {
		if key, _ := ev.Runner[livejournal.EmitKeyRunnerField].(string); key == stragglerKey {
			published = true
		}
	}
	if !published {
		t.Fatal("acknowledged emit is not readable from the published run directory")
	}
}

// TestReconcilerBackfillNeverStrandsAStragglerEmit is the #3529 major's probe
// interleaving: a straggler emit arrives while the crash-orphan backfill is
// mid-pass (during its projection query) — the window the old point-in-time
// IsOpen peek left open, where the emit rehydrated the very directory the
// backfill then deleted, acknowledging appends into an unlinked inode. The
// reservation parks the emit for the duration of the repair; it then
// re-derives state under the run-dir lock and is refused by the backfilled
// terminal journal. The invariant: an emit is either refused, or its event is
// readable from the published run directory — never acknowledged and lost.
func TestReconcilerBackfillNeverStrandsAStragglerEmit(t *testing.T) {
	proj := executeForProjection(t, projectionInput("live-straggle", crSpec("implement", []apiv1.Task{crTask("implement", "")}, nil)), &Activities{
		Det:        &scriptedStages{},
		Workspaces: testWorkspaces(t),
	}, false)
	writer, runsDir := newLiveWriter(t)
	liveAuthor(t, writer, proj, len(proj.Ops)-1)
	// The daemon "died": the journal is partial and unlocked.
	writer.Close()

	var divergences []string
	reconciler, fake, _ := reconcilerFor(t, proj, runsDir, writer, &divergences)

	runID := proj.Identity.RunID
	stragglerKey := runID + "|straggler"
	type emitResult struct {
		resp livejournal.EmitResponse
		err  error
	}
	emitted := make(chan emitResult, 1)
	var fired sync.Once
	fake.onQuery = func() {
		fired.Do(func() {
			go func() {
				resp, err := writer.Emit(context.Background(), livejournal.EmitRequest{
					RunID:  runID,
					Gaggle: proj.Identity.Gaggle,
					Ops: []livejournal.Op{{Kind: livejournal.OpAppend, Key: stragglerKey, Time: time.Date(2026, 8, 22, 4, 1, 0, 0, time.UTC), Event: &journal.Event{
						Type: journal.EventError, Error: &journal.ErrorDetail{Code: "straggler", Message: "late emission"},
					}}},
				})
				emitted <- emitResult{resp, err}
			}()
			// Give the emit time to reach the writer while the backfill is
			// still mid-query. With the reservation, the emit parks here
			// regardless of how much of this pause it uses.
			time.Sleep(200 * time.Millisecond)
		})
	}
	if _, err := reconciler.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	res := <-emitted

	events := liveEvents(t, runsDir, runID)
	if last := events[len(events)-1]; last.Type != journal.EventRunFinished {
		t.Fatalf("published journal ends with %s, want the backfilled run.finished", last.Type)
	}
	stragglerPublished := false
	for _, ev := range events {
		if key, _ := ev.Runner[livejournal.EmitKeyRunnerField].(string); key == stragglerKey {
			stragglerPublished = true
		}
	}
	if res.err == nil && res.resp.Applied > 0 && !stragglerPublished {
		t.Fatalf("straggler emit was acknowledged (%+v) but its event is absent from the published run directory", res.resp)
	}
	if res.err != nil && !errors.Is(res.err, livejournal.ErrTerminal) {
		t.Fatalf("parked straggler emit = %v, want ErrTerminal against the backfilled terminal journal", res.err)
	}
}

// TestReconcilerBackfillsCrashOrphanedLiveJournalVisibly: an INCOMPLETE
// live-authored journal of a closed run (the daemon died before the terminal
// emission) is the crash-orphan case DS5 retains projection for — it is
// backfilled from history, and the repair is announced through the
// divergence channel, never silent.
func TestReconcilerBackfillsCrashOrphanedLiveJournalVisibly(t *testing.T) {
	proj := executeForProjection(t, projectionInput("live-orphan", crSpec("implement", []apiv1.Task{crTask("implement", "")}, nil)), &Activities{
		Det:        &scriptedStages{},
		Workspaces: testWorkspaces(t),
	}, false)
	writer, runsDir := newLiveWriter(t)
	liveAuthor(t, writer, proj, len(proj.Ops)-1)
	// The daemon "dies": the writer releases the journal (and its lock)
	// without the terminal ever landing.
	writer.Close()

	var divergences []string
	reconciler, _, _ := reconcilerFor(t, proj, runsDir, stubRegistry(false), &divergences)
	if count, err := reconciler.Reconcile(context.Background()); err != nil || count != 1 {
		t.Fatalf("Reconcile = (%d, %v), want the orphan backfilled", count, err)
	}
	if len(divergences) != 1 || !strings.Contains(divergences[0], "backfilled") {
		t.Fatalf("divergences = %v, want the visible backfill annotation", divergences)
	}
	events := liveEvents(t, runsDir, proj.Identity.RunID)
	last := events[len(events)-1]
	if last.Type != journal.EventRunFinished {
		t.Fatalf("backfilled journal ends with %s, want run.finished", last.Type)
	}
	if livejournal.Authored(events) {
		t.Fatal("backfilled journal still reads as live-authored; the re-projection must carry no emit keys")
	}
}
