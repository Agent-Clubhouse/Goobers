package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/temporaltest"
)

const journalOwnerTestVersion = "engine-branch-journal-owner-v1"

// Assembly remains test-only until a versioned static walk owns branch lifetimes.
func testJournalOwner(ctx workflow.Context, runID string) (*runJournal, *runJournal, *runJournal) {
	root := &runJournal{live: true, proj: JournalProjection{Identity: journal.RunIdentity{RunID: runID, Gaggle: "web", Workflow: "owner", WorkflowVersion: 1, WorkflowDigest: "sha256:test"}, Graph: json.RawMessage(`{}`), Definition: json.RawMessage(`{}`)}}
	owner := &journalCommitOwner{root: root, mu: workflow.NewMutex(ctx)}
	root.owner = owner
	a := &runJournal{live: true, branch: 1, owner: owner, proj: root.proj}
	b := &runJournal{live: true, branch: 2, owner: owner, proj: root.proj}
	return root, a, b
}

func journalOwnerWorkflow(ctx workflow.Context, mode string) (JournalProjection, error) {
	if workflow.GetVersion(ctx, journalOwnerTestVersion, workflow.DefaultVersion, 1) == workflow.DefaultVersion {
		return JournalProjection{}, errors.New("legacy branch refusal")
	}
	root, a, b := testJournalOwner(ctx, "journal-owner-"+mode)
	if err := workflow.SetQueryHandler(ctx, JournalQuery, func() (JournalProjection, error) { return root.projectionSnapshot(), nil }); err != nil {
		return JournalProjection{}, err
	}
	root.runStarted(ctx)
	if err := root.emitPending(ctx); err != nil {
		return JournalProjection{}, err
	}
	done := 0
	var failure error
	workflow.Go(ctx, func(ctx workflow.Context) {
		mark := a.mark()
		a.append(ctx, journal.Event{Type: journal.EventStageStarted, Stage: "alpha", Attempt: 1})
		err := a.emitPending(ctx)
		if mode == "success" || mode == "cancel" {
			failure = err
		} else if err == nil {
			failure = errors.New("expected uncertain emit")
		}
		a.rollbackUnemitted(mark)
		if len(a.proj.Ops) != 1 {
			failure = errors.New("rollback erased submitted branch")
		}
		done++
	})
	workflow.Go(ctx, func(ctx workflow.Context) {
		if err := workflow.Await(ctx, func() bool { return root.owner.submitted == 2 }); err != nil {
			failure = err
			done++
			return
		}
		mark := b.mark()
		b.append(ctx, journal.Event{Type: journal.EventRunnerAnnotation, Runner: map[string]any{"kind": "discard"}})
		root.append(ctx, journal.Event{Type: journal.EventRunnerAnnotation, Runner: map[string]any{"kind": "keep-root"}})
		root.assignEmitKeys()
		b.rollbackUnemitted(mark)
		b.append(ctx, journal.Event{Type: journal.EventStageStarted, Stage: "beta", Attempt: 1})
		if mode == "cancel" {
			cancelled, cancel := workflow.WithCancel(ctx)
			cancel()
			if err := b.emitPending(cancelled); err == nil {
				failure = errors.New("cancelled waiter acquired commit lock")
			}
			b.rollbackUnemitted(mark)
		} else if err := b.emitPending(ctx); err != nil {
			failure = err
		}
		done++
	})
	if err := workflow.Await(ctx, func() bool { return done == 2 }); err != nil {
		return JournalProjection{}, err
	}
	if failure != nil {
		return JournalProjection{}, failure
	}
	root.append(ctx, journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)})
	if err := root.emitPending(ctx); err != nil {
		return JournalProjection{}, err
	}
	snapshot := root.projectionSnapshot()
	if root.owner.emitted != len(snapshot.Ops) || root.emitted != len(root.proj.Ops) || a.emitted != len(a.proj.Ops) || b.emitted != len(b.proj.Ops) {
		return JournalProjection{}, errors.New("commit watermarks diverged")
	}
	return snapshot, nil
}

type ownerEmitter struct {
	mu      sync.Mutex
	writer  *livejournal.Writer
	mode    string
	failed  bool
	active  int
	overlap bool
	batches []livejournal.EmitRequest
}

func (e *ownerEmitter) Emit(ctx context.Context, req livejournal.EmitRequest) (livejournal.EmitResponse, error) {
	e.mu.Lock()
	e.active++
	e.overlap = e.overlap || e.active > 1
	e.batches = append(e.batches, req)
	fail := !e.failed && len(req.Ops) > 0 && req.Ops[0].Branch == 1 && (e.mode == "ack-loss" || e.mode == "before-write" || e.mode == "bad-ack")
	if fail {
		e.failed = true
	}
	e.mu.Unlock()
	defer func() { e.mu.Lock(); e.active--; e.mu.Unlock() }()
	if fail && e.mode == "before-write" {
		return livejournal.EmitResponse{}, temporal.NewNonRetryableApplicationError("transport failed before write", "test_failure", nil)
	}
	response, err := e.writer.Emit(ctx, req)
	if err == nil && fail && e.mode == "bad-ack" {
		return livejournal.EmitResponse{}, nil
	}
	if err == nil && fail {
		return livejournal.EmitResponse{}, temporal.NewNonRetryableApplicationError("ack lost after commit", "test_failure", nil)
	}
	return response, err
}

func TestJournalOwnerBranchRollbackAndUncertainDelivery(t *testing.T) {
	for _, mode := range []string{"success", "ack-loss", "before-write", "bad-ack", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			var suite testsuite.WorkflowTestSuite
			env := temporaltest.NewWorkflowEnvironment(&suite)
			writer, dir := newLiveWriter(t)
			emitter := &ownerEmitter{writer: writer, mode: mode}
			env.RegisterActivityWithOptions(emitter.Emit, activity.RegisterOptions{Name: ActEmitJournal})
			env.ExecuteWorkflow(journalOwnerWorkflow, mode)
			if err := env.GetWorkflowError(); err != nil {
				t.Fatal(err)
			}
			var projection JournalProjection
			if err := env.GetWorkflowResult(&projection); err != nil {
				t.Fatal(err)
			}
			assertJournalOwnerOutcome(t, projection, liveEvents(t, dir, projection.Identity.RunID), mode)
			if emitter.overlap {
				t.Fatal("commit activities overlapped")
			}
			if mode == "ack-loss" || mode == "before-write" || mode == "bad-ack" {
				if len(emitter.batches) < 3 || emitter.batches[1].Ops[0].Key != emitter.batches[2].Ops[0].Key {
					t.Fatal("uncertain retry changed original key")
				}
			}
		})
	}
}

func assertJournalOwnerOutcome(t *testing.T, projection JournalProjection, events []journal.Event, mode string) {
	t.Helper()
	want := 5
	if mode == "cancel" {
		want = 4
	}
	if len(projection.Ops) != want || len(events) != want {
		t.Fatalf("lost/duplicated operations: projection=%d journal=%d want=%d", len(projection.Ops), len(events), want)
	}
	seen := map[string]bool{}
	for i, event := range events {
		op := projection.Ops[i]
		if event.Type != op.Event.Type || event.Branch != op.Event.Branch || event.Stage != op.Event.Stage {
			t.Fatalf("projection order drift at %d: %+v %+v", i, event, op)
		}
		if event.Runner["kind"] == "discard" {
			t.Fatal("branch rollback retained unsubmitted garbage")
		}
		if seen[op.EmitKey] {
			t.Fatalf("duplicate key %q", op.EmitKey)
		}
		seen[op.EmitKey] = true
	}
	if events[1].Branch != 1 || events[1].Stage != "alpha" || events[2].Branch != 0 || events[2].Runner["kind"] != "keep-root" {
		t.Fatalf("sibling/root decisions lost: %+v", events)
	}
	if mode != "cancel" && (events[3].Branch != 2 || events[3].Stage != "beta") {
		t.Fatal("second branch lost attribution")
	}
	repaired, err := writeTestJournal(t, filepath.Join(t.TempDir(), "projection"), projection)
	if err != nil {
		t.Fatal(err)
	}
	if got := branchJournalView(t, repaired); got[fmt.Sprintf("1/%s", journal.EventStageStarted)] == "" {
		t.Fatal("repair lost submitted branch")
	}
}

func TestJournalOwnerLegacySeamRefusesWithoutCommands(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&suite)
	env.OnGetVersion(journalOwnerTestVersion, workflow.DefaultVersion, 1).Return(workflow.DefaultVersion)
	env.ExecuteWorkflow(journalOwnerWorkflow, "success")
	if err := env.GetWorkflowError(); err == nil || !strings.Contains(err.Error(), "legacy branch refusal") {
		t.Fatalf("legacy refusal changed: %v", err)
	}
}

func TestJournalOwnerTerminalCauseUsesAggregatePosition(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&suite)
	writer, dir := newLiveWriter(t)
	env.RegisterActivityWithOptions(writer.Emit, activity.RegisterOptions{Name: ActEmitJournal})
	env.ExecuteWorkflow(func(ctx workflow.Context) (JournalProjection, error) {
		root, a, _ := testJournalOwner(ctx, "journal-owner-cause")
		root.runStarted(ctx)
		root.append(ctx, journal.Event{Type: journal.EventRunnerAnnotation, Runner: map[string]any{"kind": "coordination"}})
		a.append(ctx, journal.Event{Type: journal.EventError, Stage: "alpha", Error: &journal.ErrorDetail{Code: "WORK_FAILED", Message: "failed"}})
		root.failureTerminalCause("alpha", "WORK_FAILED", "failed")
		cause, err := root.buildTerminalCause(journal.PhaseFailed, "alpha")
		if err != nil {
			return JournalProjection{}, err
		}
		if cause.CausalEventSeq != 3 || cause.CausalEmitKey != a.proj.Ops[0].EmitKey {
			return JournalProjection{}, fmt.Errorf("cause points outside branch: %+v", cause)
		}
		root.append(ctx, journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseFailed), TerminalCause: cause})
		if err := root.emitPending(ctx); err != nil {
			return JournalProjection{}, err
		}
		return root.projectionSnapshot(), nil
	})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	events := liveEvents(t, dir, "journal-owner-cause")
	cause := events[len(events)-1].TerminalCause
	if cause == nil || cause.CausalEventSeq != events[2].Seq || events[2].Branch != 1 {
		t.Fatalf("durable cause drift: %+v", events)
	}
}
