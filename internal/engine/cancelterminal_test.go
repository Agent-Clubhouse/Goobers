package engine

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"go.temporal.io/sdk/testsuite"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/temporaltest"
)

// cancelterminal_test.go covers the terminal a CANCELLED engine run writes.
//
// The daemon's stalled-run sweep settles a wedged engine run by calling
// CancelWorkflow (cmd/goobers/stalledruns.go) instead of forging a terminal
// into a journal whose workflow is still executing. That is only a settlement
// if the cancellation itself produces one: a run left journal.PhaseRunning
// forever is unprojectable (ErrUnprojectable: "history has no terminal
// run.finished event"), stays "running" in the read model, holds its
// concurrency slot across every daemon restart, and gets re-cancelled by every
// later sweep tick — while `run abort`, the only command that could close it,
// is refused precisely because the run is engine-driven.

// blockingStage holds a stage open until its activity context is cancelled,
// standing in for the wedged stage a stall sweep finds.
type blockingStage struct{}

func (blockingStage) Run(ctx context.Context, _ apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	<-ctx.Done()
	return apiv1.ResultEnvelope{}, ctx.Err()
}

// executeCanceled runs one fixture and cancels the workflow while its stage is
// still open, returning the projection the cancelled run left behind.
func executeCanceled(t *testing.T, in RunInput, acts *Activities) JournalProjection {
	t.Helper()
	var ts testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&ts)
	env.SetStartTime(time.Date(2026, 8, 22, 3, 4, 5, 0, time.UTC))
	env.RegisterActivity(acts)
	env.RegisterDelayedCallback(env.CancelWorkflow, time.Second)
	env.ExecuteWorkflow(Run, in)
	if err := env.GetWorkflowError(); err == nil {
		t.Fatal("cancelled workflow returned no error; the run must still fail its workflow")
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

// TestCanceledEngineRunWritesItsTerminal is the pairing half of the stall
// sweep's CancelWorkflow. Cancellation used to be the one walk exit that wrote
// no terminal at all, which made the sweep's cancel a request that settled
// nothing.
func TestCanceledEngineRunWritesItsTerminal(t *testing.T) {
	in := projectionInput("run-canceled", crSpec("implement",
		[]apiv1.Task{crTask("implement", "")}, nil))

	proj := executeCanceled(t, in, &Activities{
		Det:        blockingStage{},
		Workspaces: testWorkspaces(t),
	})

	if len(proj.Ops) == 0 {
		t.Fatal("cancelled run produced no journal ops at all")
	}
	last := proj.Ops[len(proj.Ops)-1]
	if last.Kind != opAppend || last.Event == nil || last.Event.Type != journal.EventRunFinished {
		t.Fatalf("last op = %+v, want a terminal run.finished append", last)
	}
	if last.Event.Status != string(journal.PhaseAborted) {
		t.Fatalf("terminal status = %q, want %q — a cancelled run ended the way `goobers run abort` ends one",
			last.Event.Status, journal.PhaseAborted)
	}
	// The cause precedes the terminal, exactly as the failure arm does, so an
	// operator reading the journal learns WHY it closed.
	var sawCause bool
	for _, op := range proj.Ops {
		if op.Event != nil && op.Event.Type == journal.EventError && op.Event.Error != nil &&
			op.Event.Error.Code == "run_failed" {
			sawCause = true
		}
	}
	if !sawCause {
		t.Fatal("cancelled run recorded no run_failed cause before its terminal")
	}

	// And the projection the repair reconciler builds is ACCEPTED: before the
	// terminal existed this returned ErrUnprojectable and the run could never
	// leave PhaseRunning on disk.
	dir, err := ProjectRun(filepath.Join(t.TempDir(), "runs"), proj)
	if err != nil {
		t.Fatalf("ProjectRun on a cancelled run: %v — the reconciler cannot repair a run it refuses to project", err)
	}
	reader, err := journal.OpenRead(dir)
	if err != nil {
		t.Fatalf("open projected journal: %v", err)
	}
	phase, err := reader.Phase()
	if err != nil {
		t.Fatalf("read projected phase: %v", err)
	}
	if phase != journal.PhaseAborted {
		t.Fatalf("projected run phase = %q, want %q — a run stuck at running is re-swept and re-cancelled forever",
			phase, journal.PhaseAborted)
	}
}

// TestCanceledLiveJournalEngineRunClosesItsJournal is the same terminal on the
// far side the daemon actually reads. A `--live-journal` run's journal is the
// file on the daemon's PVC that the stall sweep scanned in the first place, so
// the cancellation has to close THAT, not only the workflow's in-memory
// projection — the emit runs on a disconnected context for exactly this
// reason, since the workflow's own context is already cancelled.
func TestCanceledLiveJournalEngineRunClosesItsJournal(t *testing.T) {
	writer, runsDir := newLiveWriter(t)
	in := projectionInput("run-canceled-live", crSpec("implement",
		[]apiv1.Task{crTask("implement", "")}, nil))
	in.LiveJournal = true

	executeCanceled(t, in, &Activities{
		Det:        blockingStage{},
		Workspaces: testWorkspaces(t),
		Journal:    writer,
	})

	reader, err := journal.OpenRead(filepath.Join(runsDir, in.RunID))
	if err != nil {
		t.Fatalf("open live journal: %v", err)
	}
	phase, err := reader.Phase()
	if err != nil {
		t.Fatalf("read live phase: %v", err)
	}
	if phase != journal.PhaseAborted {
		t.Fatalf("live run phase after cancellation = %q, want %q — the sweep's CancelWorkflow settled nothing on disk",
			phase, journal.PhaseAborted)
	}
}
