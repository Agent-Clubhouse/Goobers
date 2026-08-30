package journal

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// TestRunPhaseTracksTheHandlesOwnAppends and TestRunClosedReportsTheReleasedHandle
// cover the two live accessors a SECOND writer on the same run journal depends
// on. livejournal.Writer.Adopt borrows an open handle to append pod-plane events
// on it; it cannot see the owner's appends any other way, and a snapshot it took
// at adoption goes stale the moment the owner terminalizes or closes the run.

func TestRunPhaseTracksTheHandlesOwnAppends(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC)
	run, err := Create(filepath.Join(dir, "runs"), RunIdentity{
		RunID: "run-phase", Workflow: "wf", WorkflowVersion: 1,
		Trigger: Trigger{Kind: TriggerSchedule},
	}, nil, WithClock(func() time.Time { return at }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })

	if got := run.Phase(); got != PhaseRunning {
		t.Fatalf("fresh handle Phase = %s, want %s", got, PhaseRunning)
	}
	// Events that are not lifecycle transitions leave the phase alone.
	if err := run.Append(Event{Type: EventStageHeartbeat, Stage: "implement", Attempt: 1}); err != nil {
		t.Fatal(err)
	}
	if got := run.Phase(); got != PhaseRunning {
		t.Fatalf("Phase after a heartbeat = %s, want %s", got, PhaseRunning)
	}
	if err := run.Append(Event{Type: EventRunFinished, Status: string(PhaseFailed)}); err != nil {
		t.Fatal(err)
	}
	if got := run.Phase(); got != PhaseFailed {
		t.Fatalf("Phase after run.finished = %s, want %s", got, PhaseFailed)
	}
	// A re-entered run is running again, and the handle says so.
	if err := run.Append(Event{Type: EventRunResumed, Target: "implement"}); err != nil {
		t.Fatal(err)
	}
	if got := run.Phase(); got != PhaseRunning {
		t.Fatalf("Phase after run.resumed = %s, want %s", got, PhaseRunning)
	}

	// Recover reconstructs the phase from the log, so a handle taken over after
	// a restart reports what the events show, not what its first append does.
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	if err := appendTerminalDirectly(t, filepath.Join(dir, "runs", "run-phase")); err != nil {
		t.Fatal(err)
	}
	recovered, _, err := Recover(filepath.Join(dir, "runs", "run-phase"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recovered.Close() })
	if got := recovered.Phase(); got != PhaseCompleted {
		t.Fatalf("recovered Phase = %s, want %s", got, PhaseCompleted)
	}
}

// appendTerminalDirectly writes a run.finished through a short-lived handle, so
// the Recover under test sees a terminal log it did not write itself.
func appendTerminalDirectly(t *testing.T, dir string) error {
	t.Helper()
	run, _, err := Recover(dir)
	if err != nil {
		return err
	}
	if err := run.Append(Event{Type: EventRunFinished, Status: string(PhaseCompleted)}); err != nil {
		return err
	}
	return run.Close()
}

func TestRunClosedReportsTheReleasedHandle(t *testing.T) {
	run, err := Create(filepath.Join(t.TempDir(), "runs"), RunIdentity{
		RunID: "run-closed", Workflow: "wf", WorkflowVersion: 1,
		Trigger: Trigger{Kind: TriggerSchedule},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if run.Closed() {
		t.Fatal("a fresh handle reports itself closed")
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	if !run.Closed() {
		t.Fatal("Closed is false after Close")
	}
	// The property a borrower relies on: Closed and ErrClosed agree, so
	// checking the flag is the same answer as attempting the append.
	if err := run.Append(Event{Type: EventStageHeartbeat, Stage: "implement"}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Append after Close = %v, want ErrClosed", err)
	}
}
