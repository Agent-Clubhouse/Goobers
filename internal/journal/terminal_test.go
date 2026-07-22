package journal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// simulateCrashInTerminalWindow hand-builds a run journal exactly to the
// point a real Append(run.finished) would have left it had the process died
// between the event's own fsync and the checkpoint rename that follows it in
// the same Append call (#242): the event log durably shows the run finished,
// but state.json still claims the run is running at some prior stage/gate.
// A clean Append (which itself checkpoints correctly) is used first so the
// event log is genuinely valid, then state.json is overwritten by hand to
// simulate the lost checkpoint write — the only part of this window that
// can actually go missing.
func simulateCrashInTerminalWindow(t *testing.T, dir string, status RunPhase, staleMachineState string) {
	t.Helper()
	stale := State{
		Schema:       StateSchema,
		RunID:        testIdentity().RunID,
		Phase:        PhaseRunning,
		MachineState: staleMachineState,
		LastSeq:      0,
		UpdatedAt:    fixedClock()(),
	}
	b, err := json.Marshal(stale)
	if err != nil {
		t.Fatalf("marshal stale state: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, fileState), b, 0o644); err != nil {
		t.Fatalf("write stale state.json: %v", err)
	}
}

// TestRecoverHealsStaleTerminalCheckpoint is #242's acceptance criterion "a
// clean-but-uncheckpointed terminal log heals state.json to the terminal
// phase": Recover must notice a reconstructed-terminal log whose on-disk
// checkpoint still claims {running, <stage/gate>} and rewrite state.json to
// match — closing the crash window a torn-tail repair alone can't catch
// (a cleanly-fsynced run.finished leaves no torn tail at all).
func TestRecoverHealsStaleTerminalCheckpoint(t *testing.T) {
	root := t.TempDir()
	run, err := Create(root, testIdentity(), nil, WithClock(fixedClock()))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := run.Append(Event{Type: EventRunFinished, Status: string(PhaseCompleted)}); err != nil {
		t.Fatalf("Append run.finished: %v", err)
	}
	if err := run.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	dir := filepath.Join(root, testIdentity().RunID)
	// Overwrite the checkpoint Append itself just wrote, simulating the
	// crash landing before that write in the real timing window.
	simulateCrashInTerminalWindow(t, dir, PhaseRunning, "some-gate")

	rd, err := OpenRead(dir)
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	preHealState, err := rd.State()
	if err != nil {
		t.Fatalf("State (pre-heal): %v", err)
	}
	if preHealState.Phase != PhaseRunning {
		t.Fatalf("sanity: pre-heal state.json phase = %q, want running (simulated stale checkpoint)", preHealState.Phase)
	}

	recovered, _, err := Recover(dir, WithClock(fixedClock()))
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if err := recovered.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	healedState, err := rd.State()
	if err != nil {
		t.Fatalf("State (post-heal): %v", err)
	}
	if healedState.Phase != PhaseCompleted {
		t.Fatalf("healed state.json phase = %q, want completed", healedState.Phase)
	}
	if healedState.MachineState != "" {
		t.Fatalf("healed state.json machineState = %q, want empty (terminal invariant)", healedState.MachineState)
	}

	// A second Recover is idempotent — no further checkpoint churn once
	// state.json already agrees with the log.
	recovered2, report2, err := Recover(dir, WithClock(fixedClock()))
	if err != nil {
		t.Fatalf("second Recover: %v", err)
	}
	_ = recovered2.Close()
	if report2.Repaired {
		t.Fatalf("second Recover reported Repaired=true, want false — nothing torn, checkpoint already healed")
	}
}

// TestReaderPhaseReconstructsFromLogNotCheckpoint is the direct unit test for
// Reader.Phase(): it must return the event-log-derived phase even when
// state.json disagrees, and must not itself mutate state.json (only Recover
// heals; Phase is read-only).
func TestReaderPhaseReconstructsFromLogNotCheckpoint(t *testing.T) {
	root := t.TempDir()
	run, err := Create(root, testIdentity(), nil, WithClock(fixedClock()))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := run.Append(Event{Type: EventRunFinished, Status: string(PhaseEscalated)}); err != nil {
		t.Fatalf("Append run.finished: %v", err)
	}
	if err := run.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	dir := filepath.Join(root, testIdentity().RunID)
	simulateCrashInTerminalWindow(t, dir, PhaseRunning, "some-gate")

	rd, err := OpenRead(dir)
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	phase, err := rd.Phase()
	if err != nil {
		t.Fatalf("Phase: %v", err)
	}
	if phase != PhaseEscalated {
		t.Fatalf("Phase() = %q, want escalated (from the log, not the stale running checkpoint)", phase)
	}
	// Phase() is read-only: state.json must be untouched.
	st, err := rd.State()
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st.Phase != PhaseRunning {
		t.Fatalf("state.json phase = %q after Phase(), want unchanged (running) — Phase() must not heal", st.Phase)
	}
}

func TestRunResumedReopensTerminalPhaseAndRecoversTarget(t *testing.T) {
	root := t.TempDir()
	run, err := Create(root, testIdentity(), nil, WithClock(fixedClock()))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := run.Append(Event{
		Type: EventRunFinished, Status: string(PhaseEscalated),
		Error: &ErrorDetail{Code: "needs_human", Message: "review exhausted"},
	}); err != nil {
		t.Fatalf("Append run.finished: %v", err)
	}
	if err := run.Append(Event{
		Type: EventRunResumed, Status: string(PhaseEscalated), Target: "implement",
		Actor: "operator@example.test", WorkflowVersion: testIdentity().WorkflowVersion,
		WorkflowDigest: testIdentity().WorkflowDigest,
	}); err != nil {
		t.Fatalf("Append run.resumed: %v", err)
	}
	if err := run.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	dir := filepath.Join(root, testIdentity().RunID)
	stale := State{
		Schema: StateSchema, RunID: testIdentity().RunID, Phase: PhaseEscalated,
		Reason: "review exhausted", LastSeq: 2, UpdatedAt: fixedClock()(),
	}
	data, err := json.Marshal(stale)
	if err != nil {
		t.Fatalf("marshal stale checkpoint: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, fileState), data, 0o644); err != nil {
		t.Fatalf("write stale checkpoint: %v", err)
	}

	reader, err := OpenRead(dir)
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	phase, err := reader.Phase()
	if err != nil {
		t.Fatalf("Phase: %v", err)
	}
	if phase != PhaseRunning {
		t.Fatalf("Phase() = %q, want running from the last lifecycle event", phase)
	}

	recovered, _, err := Recover(dir, WithClock(fixedClock()))
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if err := recovered.Close(); err != nil {
		t.Fatalf("Close recovered: %v", err)
	}
	state, err := reader.State()
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if state.Phase != PhaseRunning || state.MachineState != "implement" || state.Reason != "" {
		t.Fatalf("recovered state = %+v, want running at implement with no terminal reason", state)
	}
}

// TestRecoverDoesNotHealNonTerminalMissingCheckpoint confirms Recover's
// healing is one-directional: when the reconstructed phase is still
// PhaseRunning, an unreadable state.json is left alone rather than
// fabricated (this package has no way to know the real MachineState without
// the workflow Machine, which only the runner package holds — see
// internal/runner.Resume's own fallback for that case).
func TestRecoverDoesNotHealNonTerminalMissingCheckpoint(t *testing.T) {
	root := t.TempDir()
	run, err := Create(root, testIdentity(), nil, WithClock(fixedClock()))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := run.Append(Event{Type: EventStageStarted, Stage: "implement", Attempt: 1}); err != nil {
		t.Fatalf("Append stage.started: %v", err)
	}
	if err := run.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	dir := filepath.Join(root, testIdentity().RunID)
	if err := os.Remove(filepath.Join(dir, fileState)); err != nil {
		t.Fatalf("remove state.json: %v", err)
	}

	recovered, report, err := Recover(dir, WithClock(fixedClock()))
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if report.Repaired {
		t.Fatalf("Repaired = true, want false — no torn tail, deliberately-missing checkpoint")
	}
	if recovered.phase != PhaseRunning {
		t.Fatalf("recovered.phase = %q, want running", recovered.phase)
	}
	if err := recovered.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, fileState)); !os.IsNotExist(err) {
		t.Fatalf("state.json exists after Recover on a non-terminal run with a missing checkpoint, want still absent: %v", err)
	}
}
