package runner

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

func createTerminalResumeRun(t *testing.T, runsDir, runID string, machine *workflow.Machine, phase journal.RunPhase) {
	t.Helper()
	jr, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: runID, Workflow: machine.Def.Name, WorkflowVersion: machine.Def.Version,
		WorkflowDigest: machine.Digest(), Gaggle: machine.Def.Spec.Gaggle,
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}
	if err := jr.Append(journal.Event{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1}); err != nil {
		t.Fatalf("append prior stage.started: %v", err)
	}
	if err := jr.Append(journal.Event{
		Type: journal.EventStageFinished, Stage: "implement", Attempt: 1,
		Status: string(apiv1.ResultFailure),
		Error:  &journal.ErrorDetail{Code: "needs_changes", Message: "human intervention required"},
	}); err != nil {
		t.Fatalf("append prior stage.finished: %v", err)
	}
	if err := jr.Append(journal.Event{Type: journal.EventRunFinished, Status: string(phase)}); err != nil {
		t.Fatalf("append run.finished: %v", err)
	}
	if err := jr.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func terminalResumeRunner(t *testing.T, runsDir, fixtureRepo string, wtMgr *worktree.Manager, det invoke.Deterministic) *Runner {
	t.Helper()
	r, err := New(Config{
		NewDeterministic: func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
			return det, nil
		},
		Automated:    gate.NewAutomatedEvaluator(),
		Worktrees:    wtMgr,
		RunsDir:      runsDir,
		RepoCloneURL: func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

func TestResumeFromTerminalIsDurableAndReexecutesTarget(t *testing.T) {
	machine := fixtureMachine(t)
	runsDir, fixtureRepo, wtMgr := newTestRunnerEnv(t)
	const runID = "run-human-resume"
	createTerminalResumeRun(t, runsDir, runID, machine, journal.PhaseEscalated)

	det := &countingDeterministic{}
	r := terminalResumeRunner(t, runsDir, fixtureRepo, wtMgr, det)
	repoRef := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := r.ResumeFromTerminal(cancelled, ResumeFromTerminalInput{
		RunID: runID, Machine: machine, RepoRef: repoRef,
		Target: "implement", Actor: "operator@example.test",
	})
	if err != nil {
		t.Fatalf("ResumeFromTerminal: %v", err)
	}
	if result.Phase != journal.PhaseRunning || result.FinalState != "implement" {
		t.Fatalf("paused result = %+v, want running at implement", result)
	}
	if det.calls != 0 {
		t.Fatalf("executor calls after cancelled action = %d, want 0", det.calls)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	phase, err := rd.Phase()
	if err != nil {
		t.Fatalf("Phase: %v", err)
	}
	if phase != journal.PhaseRunning {
		t.Fatalf("phase after action = %q, want running", phase)
	}
	state, err := rd.State()
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if state.MachineState != "implement" {
		t.Fatalf("machine state = %q, want implement", state.MachineState)
	}

	result, err = r.Resume(context.Background(), ResumeInput{RunID: runID, Machine: machine, RepoRef: repoRef})
	if err != nil {
		t.Fatalf("crash Resume: %v", err)
	}
	if result.Phase != journal.PhaseCompleted {
		t.Fatalf("resumed result = %+v, want completed", result)
	}
	if det.calls != 1 {
		t.Fatalf("executor calls = %d, want 1 reexecution of the human target", det.calls)
	}

	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var resumed journal.Event
	var firstTerminal, secondTerminal int
	for _, event := range events {
		switch event.Type {
		case journal.EventRunResumed:
			resumed = event
		case journal.EventRunFinished:
			if event.Status == string(journal.PhaseEscalated) {
				firstTerminal++
			}
			if event.Status == string(journal.PhaseCompleted) {
				secondTerminal++
			}
		}
	}
	if resumed.Actor != "operator@example.test" ||
		resumed.Target != "implement" ||
		resumed.Status != string(journal.PhaseEscalated) ||
		resumed.WorkflowVersion != machine.Def.Version ||
		resumed.WorkflowDigest != machine.Digest() {
		t.Fatalf("run.resumed = %+v, want actor, target, prior phase, and immutable workflow pin", resumed)
	}
	if firstTerminal != 1 || secondTerminal != 1 {
		t.Fatalf("terminal events: escalated=%d completed=%d, want one of each", firstTerminal, secondTerminal)
	}
}

func TestResumeFromTerminalRefusesChangedWorkflowPin(t *testing.T) {
	machine := fixtureMachine(t)
	runsDir, fixtureRepo, wtMgr := newTestRunnerEnv(t)
	const runID = "run-human-resume-pin"
	createTerminalResumeRun(t, runsDir, runID, machine, journal.PhaseFailed)

	changed, err := workflow.Compile(workflow.Definition{
		Name: machine.Def.Name, Version: machine.Def.Version,
		Spec: apiv1.WorkflowSpec{
			Gaggle: machine.Def.Spec.Gaggle, Start: "implement",
			Tasks: []apiv1.Task{{
				Name: "implement", Type: apiv1.TaskDeterministic, Goal: "changed",
				Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: workflow.TerminalComplete,
			}},
		},
	})
	if err != nil {
		t.Fatalf("compile changed machine: %v", err)
	}

	det := &countingDeterministic{}
	r := terminalResumeRunner(t, runsDir, fixtureRepo, wtMgr, det)
	_, err = r.ResumeFromTerminal(context.Background(), ResumeFromTerminalInput{
		RunID: runID, Machine: changed,
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
		Target:  "implement", Actor: "operator@example.test",
	})
	if err == nil || !strings.Contains(err.Error(), "WF-016") {
		t.Fatalf("ResumeFromTerminal error = %v, want WF-016 pin refusal", err)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	phase, err := rd.Phase()
	if err != nil {
		t.Fatalf("Phase: %v", err)
	}
	if phase != journal.PhaseFailed {
		t.Fatalf("phase after refused action = %q, want failed", phase)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	for _, event := range events {
		if event.Type == journal.EventRunResumed {
			t.Fatalf("refused action journaled run.resumed: %+v", event)
		}
	}
}

func TestResumeFromTerminalAcceptsFailedRunAndGateTarget(t *testing.T) {
	machine := fixtureMachine(t)
	runsDir, fixtureRepo, wtMgr := newTestRunnerEnv(t)
	const runID = "run-human-resume-failed"
	createTerminalResumeRun(t, runsDir, runID, machine, journal.PhaseFailed)

	r := terminalResumeRunner(t, runsDir, fixtureRepo, wtMgr, &countingDeterministic{})
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := r.ResumeFromTerminal(cancelled, ResumeFromTerminalInput{
		RunID: runID, Machine: machine,
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
		Target:  "review", Actor: "operator@example.test",
	})
	if err != nil {
		t.Fatalf("ResumeFromTerminal: %v", err)
	}
	if result.Phase != journal.PhaseRunning || result.FinalState != "review" {
		t.Fatalf("result = %+v, want failed run reopened at review gate", result)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	resumed := events[len(events)-1]
	if resumed.Type != journal.EventRunResumed ||
		resumed.Status != string(journal.PhaseFailed) ||
		resumed.Target != "review" {
		t.Fatalf("last event = %+v, want failed run.resumed at review", resumed)
	}
}

func TestResumeFromTerminalValidatesActionBeforeJournalAccess(t *testing.T) {
	machine := fixtureMachine(t)
	r := &Runner{}
	tests := []struct {
		name string
		in   ResumeFromTerminalInput
		want string
	}{
		{
			name: "invalid run id",
			in: ResumeFromTerminalInput{
				RunID: "../escape", Machine: machine, Target: "implement", Actor: "operator",
			},
			want: "invalid run id",
		},
		{
			name: "missing machine",
			in: ResumeFromTerminalInput{
				RunID: "valid-run", Target: "implement", Actor: "operator",
			},
			want: "Machine is required",
		},
		{
			name: "missing target",
			in: ResumeFromTerminalInput{
				RunID: "valid-run", Machine: machine, Actor: "operator",
			},
			want: "target is required",
		},
		{
			name: "missing actor",
			in: ResumeFromTerminalInput{
				RunID: "valid-run", Machine: machine, Target: "implement",
			},
			want: "actor is required",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := r.ResumeFromTerminal(context.Background(), test.in)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ResumeFromTerminal error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestCurrentRunSegmentResetsGateRecoveryState(t *testing.T) {
	events := []journal.Event{
		{
			Type: journal.EventGateStarted, Gate: "review",
			Runner: map[string]any{"repassAttempt": float64(3)},
		},
		{
			Type: journal.EventGateEvaluated, Gate: "review",
			Runner: map[string]any{"repassAttempt": float64(3), "diffDigest": "sha256:old"},
		},
		{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)},
		{Type: journal.EventRunResumed, Target: "implement"},
	}
	segment, target := currentRunSegment(events)
	if target != "implement" || len(segment) != 0 {
		t.Fatalf("current segment = (%q, %+v), want empty segment at implement", target, segment)
	}
	if attempts := gateRepassSeed(segment); attempts != nil {
		t.Fatalf("gate attempts = %+v, want fresh budget after human resume", attempts)
	}
	if digests := gateDiffSeed(segment); digests != nil {
		t.Fatalf("gate diff digests = %+v, want fresh convergence state after human resume", digests)
	}
}
