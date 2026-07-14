package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
)

// overwriteStateJSON replaces runDir/state.json with a hand-built, possibly
// stale checkpoint — the durable half of #242's crash window (a run.finished
// event fsynced, but the checkpoint rewrite that follows it in the same
// Append call never landed).
func overwriteStateJSON(t *testing.T, runDir string, st journal.State) {
	t.Helper()
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "state.json"), b, 0o644); err != nil {
		t.Fatalf("write state.json: %v", err)
	}
}

// TestRunnerResumeTrustsJournalOverStaleTerminalCheckpoint is #242's primary
// acceptance scenario: a run whose event log durably shows run.finished, but
// whose state.json checkpoint still claims {running, <gate>} (the crash
// landed between the event's fsync and the checkpoint rewrite inside the
// same Append call), must resume to a no-op reporting the JOURNALED terminal
// phase — zero executor dispatches, exactly one run.finished in the log. The
// pre-#242 bug trusted state.json directly here, which could re-evaluate a
// gate fresh and re-dispatch already-completed side-effecting stages.
func TestRunnerResumeTrustsJournalOverStaleTerminalCheckpoint(t *testing.T) {
	machine := fixtureMachine(t)
	runsDir, fixtureRepo, wtMgr := newTestRunnerEnv(t)

	jr, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: "run-stale-terminal", Workflow: machine.Def.Name, WorkflowVersion: machine.Def.Version,
		WorkflowDigest: machine.Digest(), Gaggle: "acme-web", Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}
	jr.SetMachineState("implement")
	if err := jr.Append(journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1}); err != nil {
		t.Fatalf("append stage.started: %v", err)
	}
	if err := jr.Append(journal.Event{Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: string(apiv1.ResultSuccess)}); err != nil {
		t.Fatalf("append stage.finished: %v", err)
	}
	if err := jr.Append(journal.Event{Type: journal.EventGateEvaluated, Gate: "review", Status: "pass"}); err != nil {
		t.Fatalf("append gate.evaluated: %v", err)
	}
	// Append's own Append(run.finished) durably fsyncs the event AND
	// checkpoints in the same call — a real crash in the #242 window would
	// leave the FIRST of those two durable but not the second. Simulate
	// that by overwriting state.json right after, rather than skipping the
	// checkpoint (there is no seam to interrupt Append mid-call from a
	// test), so the on-disk end state is identical either way.
	if err := jr.Append(journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseCompleted)}); err != nil {
		t.Fatalf("append run.finished: %v", err)
	}
	if err := jr.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	runDir := filepath.Join(runsDir, "run-stale-terminal")
	overwriteStateJSON(t, runDir, journal.State{
		Schema: journal.StateSchema, RunID: "run-stale-terminal",
		Phase: journal.PhaseRunning, MachineState: "review",
	})

	det := &countingDeterministic{}
	r, err := New(Config{
		NewDeterministic: func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) { return det, nil },
		Automated:        gate.NewAutomatedEvaluator(),
		Worktrees:        wtMgr,
		RunsDir:          runsDir,
		RepoCloneURL:     func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := r.Resume(context.Background(), ResumeInput{
		RunID:   "run-stale-terminal",
		Machine: machine,
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed (the journaled terminal phase, not the stale checkpoint's \"running\")", res.Phase)
	}
	if det.calls != 0 {
		t.Fatalf("executor dispatched %d times, want 0 — the run was already terminal, resume must be a pure no-op", det.calls)
	}

	rd, err := journal.OpenRead(runDir)
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var finished, gateEvals int
	for _, e := range events {
		if e.Type == journal.EventRunFinished {
			finished++
		}
		if e.Type == journal.EventGateEvaluated {
			gateEvals++
		}
	}
	if finished != 1 {
		t.Fatalf("run.finished count = %d, want exactly 1 — Resume must not append a second terminal event onto an already-finished run", finished)
	}
	if gateEvals != 1 {
		t.Fatalf("gate.evaluated count = %d, want exactly 1 (the pre-crash pass only) — a no-op resume must not re-evaluate the gate", gateEvals)
	}
}

// TestRunnerResumeSurvivesMissingStateJSONAfterFinishedTask is #242's second
// acceptance scenario applied to the mid-transition crash window (#107's
// timing): a run whose last finished task's next-state transition was never
// checkpointed AND whose state.json is now entirely missing/corrupt must
// still resume correctly — falling back to the last really-finished stage's
// own name (exactly what state.json would have shown in that exact crash
// window, per the SetMachineState-timing note in resume.go), not failing
// outright the way a hard state.json read requirement used to.
func TestRunnerResumeSurvivesMissingStateJSONAfterFinishedTask(t *testing.T) {
	machine := fixtureMachine(t)
	runsDir, fixtureRepo, wtMgr := newTestRunnerEnv(t)

	jr, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: "run-missing-state", Workflow: machine.Def.Name, WorkflowVersion: machine.Def.Version,
		WorkflowDigest: machine.Digest(), Gaggle: "acme-web", Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}
	jr.SetMachineState("implement")
	if err := jr.Append(journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1}); err != nil {
		t.Fatalf("append stage.started: %v", err)
	}
	if err := jr.Append(journal.Event{Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: string(apiv1.ResultSuccess)}); err != nil {
		t.Fatalf("append stage.finished: %v", err)
	}
	// No further events: the crash lands here, before walk's next loop
	// iteration ever reassigns state to "review" (#107's timing).
	if err := jr.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	runDir := filepath.Join(runsDir, "run-missing-state")
	if err := os.Remove(filepath.Join(runDir, "state.json")); err != nil {
		t.Fatalf("remove state.json: %v", err)
	}

	counting := &countingDeterministic{}
	r, err := New(Config{
		NewDeterministic: func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) { return counting, nil },
		Automated:        gate.NewAutomatedEvaluator(),
		Worktrees:        wtMgr,
		RunsDir:          runsDir,
		RepoCloneURL:     func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := r.Resume(context.Background(), ResumeInput{
		RunID:   "run-missing-state",
		Machine: machine,
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", res.Phase)
	}
	if counting.calls != 0 {
		t.Fatalf("implement was re-dispatched %d times, want 0 — an already-finished stage must never re-run its side effects even with no checkpoint to consult", counting.calls)
	}
}

// TestRunnerResumeSurvivesMissingStateJSONBeforeAnyStage covers #242's
// fallback when NO stage has ever finished (a crash between Start's initial
// ref.touched append and the walk's very first dispatch) and state.json is
// missing/corrupt: Resume must fall back to the machine's own declared start
// state, exactly where Start() itself would have begun.
func TestRunnerResumeSurvivesMissingStateJSONBeforeAnyStage(t *testing.T) {
	machine := fixtureMachine(t)
	runsDir, fixtureRepo, wtMgr := newTestRunnerEnv(t)

	jr, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: "run-missing-state-fresh", Workflow: machine.Def.Name, WorkflowVersion: machine.Def.Version,
		WorkflowDigest: machine.Digest(), Gaggle: "acme-web", Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}
	// No SetMachineState, no stage events at all — the crash lands before
	// walk's very first loop iteration.
	if err := jr.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	runDir := filepath.Join(runsDir, "run-missing-state-fresh")
	if err := os.Remove(filepath.Join(runDir, "state.json")); err != nil {
		t.Fatalf("remove state.json: %v", err)
	}

	det := &countingDeterministic{}
	r, err := New(Config{
		NewDeterministic: func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) { return det, nil },
		Automated:        gate.NewAutomatedEvaluator(),
		Worktrees:        wtMgr,
		RunsDir:          runsDir,
		RepoCloneURL:     func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := r.Resume(context.Background(), ResumeInput{
		RunID:   "run-missing-state-fresh",
		Machine: machine,
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", res.Phase)
	}
	if det.calls != 1 {
		t.Fatalf("implement dispatched %d times, want exactly 1 — fallback to the machine's declared start state must still run the whole workflow", det.calls)
	}
}
