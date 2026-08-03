package runner

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

func overrideFixtureMachine(t *testing.T, evaluator apiv1.EvaluatorKind) *workflow.Machine {
	t.Helper()
	gateSpec := apiv1.Gate{
		Name: "review", Evaluator: evaluator,
		Branches: map[string]string{
			"pass":          workflow.TerminalComplete,
			"needs-changes": "fix",
			"fail":          workflow.TargetAbort,
		},
	}
	tasks := []apiv1.Task{{
		Name: "fix", Type: apiv1.TaskDeterministic, Goal: "apply requested changes",
		Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: workflow.TerminalComplete,
	}}
	if evaluator == apiv1.EvaluatorAgentic {
		gateSpec.Agentic = &apiv1.AgenticGate{Goober: "reviewer"}
	} else {
		gateSpec.Automated = &apiv1.AutomatedGate{Check: "status-equals"}
		delete(gateSpec.Branches, "needs-changes")
		tasks = nil
	}
	machine, err := workflow.Compile(workflow.Definition{
		Name:    "override-review",
		Version: 1,
		Spec: apiv1.WorkflowSpec{
			Gaggle: "acme-web",
			Start:  "review",
			Tasks:  tasks,
			Gates:  []apiv1.Gate{gateSpec},
		},
	}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile override fixture: %v", err)
	}
	return machine
}

func createEscalatedGateRun(t *testing.T, runsDir, runID string, machine *workflow.Machine) {
	createTerminalGateRun(t, runsDir, runID, machine, "needs-changes", workflow.TargetEscalate, journal.PhaseEscalated)
}

func createTerminalGateRun(t *testing.T, runsDir, runID string, machine *workflow.Machine, verdict, target string, phase journal.RunPhase) {
	t.Helper()
	jr, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: runID, Workflow: machine.Def.Name, WorkflowVersion: machine.Def.Version,
		WorkflowDigest: machine.Digest(), Gaggle: machine.Def.Spec.Gaggle,
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}
	if err := jr.Append(journal.Event{
		Type: journal.EventGateEvaluated, Gate: "review", Verdict: verdict,
		Target: target, Escalated: target == workflow.TargetEscalate,
	}); err != nil {
		t.Fatalf("append gate verdict: %v", err)
	}
	if err := jr.Append(journal.Event{Type: journal.EventRunFinished, Status: string(phase)}); err != nil {
		t.Fatalf("append terminal: %v", err)
	}
	if err := jr.Close(); err != nil {
		t.Fatalf("close run: %v", err)
	}
}

func TestOverrideGateReopensAbortedRunAndAdvancesSelectedBranch(t *testing.T) {
	machine := overrideFixtureMachine(t, apiv1.EvaluatorAgentic)
	runsDir, fixtureRepo, wtMgr := newTestRunnerEnv(t)
	const runID = "override-aborted"
	createTerminalGateRun(t, runsDir, runID, machine, "fail", workflow.TargetAbort, journal.PhaseAborted)

	det := &countingDeterministic{}
	r := terminalResumeRunner(t, runsDir, fixtureRepo, wtMgr, det)
	result, err := r.OverrideGate(context.Background(), OverrideGateInput{
		RunID: runID, Machine: machine,
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
		Gate:    "review", Verdict: "needs-changes", Actor: "maintainer@example.test",
		Rationale: "The failure was reviewed and needs a corrective pass.",
	})
	if err != nil {
		t.Fatalf("OverrideGate: %v", err)
	}
	if result.Phase != journal.PhaseCompleted || det.calls != 1 {
		t.Fatalf("result = %+v, deterministic calls = %d; want completed after one corrective task", result, det.calls)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var override journal.Event
	for _, event := range events {
		if event.Type == journal.EventGateOverridden {
			override = event
		}
	}
	if override.Status != string(journal.PhaseAborted) ||
		override.Target != "fix" ||
		override.Rationale != "The failure was reviewed and needs a corrective pass." {
		t.Fatalf("gate.overridden = %+v", override)
	}
}

func TestOverrideGateAdvancesConfiguredBranchAndJournalsRationale(t *testing.T) {
	machine := overrideFixtureMachine(t, apiv1.EvaluatorAgentic)
	runsDir, fixtureRepo, wtMgr := newTestRunnerEnv(t)
	repoRef := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"}

	tests := []struct {
		name      string
		verdict   string
		wantCalls int
	}{
		{name: "pass", verdict: "pass"},
		{name: "another branch", verdict: "needs-changes", wantCalls: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runID := "override-" + strings.ReplaceAll(test.name, " ", "-")
			createEscalatedGateRun(t, runsDir, runID, machine)
			det := &countingDeterministic{}
			r := terminalResumeRunner(t, runsDir, fixtureRepo, wtMgr, det)

			result, err := r.OverrideGate(context.Background(), OverrideGateInput{
				RunID: runID, Machine: machine, RepoRef: repoRef,
				Gate: "review", Verdict: test.verdict, Actor: "maintainer@example.test",
				Rationale: "The reviewer finding was manually resolved.",
			})
			if err != nil {
				t.Fatalf("OverrideGate: %v", err)
			}
			if result.Phase != journal.PhaseCompleted {
				t.Fatalf("result = %+v, want completed", result)
			}
			if det.calls != test.wantCalls {
				t.Fatalf("deterministic calls = %d, want %d", det.calls, test.wantCalls)
			}

			rd, err := journal.OpenRead(filepath.Join(runsDir, runID))
			if err != nil {
				t.Fatalf("OpenRead: %v", err)
			}
			events, err := rd.Events()
			if err != nil {
				t.Fatalf("Events: %v", err)
			}
			var override journal.Event
			for _, event := range events {
				if event.Type == journal.EventGateOverridden {
					override = event
				}
			}
			wantTarget := machine.Def.Spec.Gates[0].Branches[test.verdict]
			if wantTarget == workflow.TerminalComplete {
				wantTarget = journal.TargetComplete
			}
			if override.Gate != "review" || override.Verdict != test.verdict ||
				override.Actor != "maintainer@example.test" ||
				override.Rationale != "The reviewer finding was manually resolved." ||
				override.Status != string(journal.PhaseEscalated) ||
				override.Target != wantTarget {
				t.Fatalf("gate.overridden = %+v", override)
			}
		})
	}
}

func TestOverrideGateRequiresRationaleAndNondeterministicGate(t *testing.T) {
	machine := overrideFixtureMachine(t, apiv1.EvaluatorAgentic)
	r := &Runner{}
	_, err := r.OverrideGate(context.Background(), OverrideGateInput{
		RunID: "override-validation", Machine: machine, Gate: "review",
		Verdict: "pass", Actor: "maintainer",
	})
	if err == nil || !strings.Contains(err.Error(), "Rationale is required") {
		t.Fatalf("missing rationale error = %v", err)
	}

	automated := overrideFixtureMachine(t, apiv1.EvaluatorAutomated)
	_, err = r.OverrideGate(context.Background(), OverrideGateInput{
		RunID: "override-validation", Machine: automated, Gate: "review",
		Verdict: "pass", Actor: "maintainer", Rationale: "manual inspection",
	})
	if err == nil || !strings.Contains(err.Error(), "deterministic") {
		t.Fatalf("automated gate error = %v", err)
	}
}

func TestCurrentTerminalGateRejectsStaleOrUnrelatedVerdicts(t *testing.T) {
	escalated := journal.Event{
		Type: journal.EventGateEvaluated, Gate: "review", Verdict: "needs-changes",
		Target: workflow.TargetEscalate, Escalated: true,
	}
	tests := []struct {
		name   string
		events []journal.Event
		phase  journal.RunPhase
		want   bool
	}{
		{name: "current escalation", events: []journal.Event{escalated}, phase: journal.PhaseEscalated, want: true},
		{name: "target records escalation", events: []journal.Event{{
			Type: journal.EventGateEvaluated, Gate: "review", Target: workflow.TargetEscalate,
		}}, phase: journal.PhaseEscalated, want: true},
		{name: "current abort", events: []journal.Event{{
			Type: journal.EventGateEvaluated, Gate: "review", Target: workflow.TargetAbort,
		}}, phase: journal.PhaseAborted, want: true},
		{name: "current completion", events: []journal.Event{{
			Type: journal.EventGateEvaluated, Gate: "review", Target: workflow.TerminalComplete,
		}}, phase: journal.PhaseCompleted, want: true},
		{name: "different gate", events: []journal.Event{{
			Type: journal.EventGateEvaluated, Gate: "security", Target: workflow.TargetEscalate,
		}}, phase: journal.PhaseEscalated},
		{name: "non-escalating verdict", events: []journal.Event{{
			Type: journal.EventGateEvaluated, Gate: "review", Target: workflow.TerminalComplete,
		}}, phase: journal.PhaseEscalated},
		{name: "newer gate verdict", events: []journal.Event{escalated, {
			Type: journal.EventGateEvaluated, Gate: "security", Target: workflow.TargetEscalate,
		}}, phase: journal.PhaseEscalated},
		{name: "after resume", events: []journal.Event{
			escalated, {Type: journal.EventRunResumed, Target: "fix"},
		}, phase: journal.PhaseEscalated},
		{name: "after override", events: []journal.Event{
			escalated, {Type: journal.EventGateOverridden, Gate: "review", Target: "fix"},
		}, phase: journal.PhaseEscalated},
		{name: "after rerun request", events: []journal.Event{
			escalated, {Type: journal.EventStageRerunRequested, Stage: "implement"},
		}, phase: journal.PhaseEscalated},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isCurrentTerminalGate(test.events, "review", test.phase); got != test.want {
				t.Fatalf("isCurrentTerminalGate() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestOverrideGateRejectsEscalationUnrelatedToGate(t *testing.T) {
	machine := overrideFixtureMachine(t, apiv1.EvaluatorAgentic)
	runsDir, fixtureRepo, wtMgr := newTestRunnerEnv(t)
	const runID = "override-unrelated-escalation"
	jr, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: runID, Workflow: machine.Def.Name, WorkflowVersion: machine.Def.Version,
		WorkflowDigest: machine.Digest(), Gaggle: machine.Def.Spec.Gaggle,
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}
	if err := jr.Append(journal.Event{
		Type: journal.EventGateEvaluated, Gate: "review", Verdict: "pass",
		Target: workflow.TerminalComplete,
	}); err != nil {
		t.Fatalf("append gate verdict: %v", err)
	}
	if err := jr.Append(journal.Event{
		Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated),
	}); err != nil {
		t.Fatalf("append unrelated escalation: %v", err)
	}
	if err := jr.Close(); err != nil {
		t.Fatalf("close run: %v", err)
	}

	r := terminalResumeRunner(t, runsDir, fixtureRepo, wtMgr, &countingDeterministic{})
	_, err = r.OverrideGate(context.Background(), OverrideGateInput{
		RunID: runID, Machine: machine, Gate: "review", Verdict: "pass",
		Actor: "maintainer", Rationale: "manual inspection",
	})
	if err == nil || !strings.Contains(err.Error(), "did not cause") {
		t.Fatalf("unrelated escalation error = %v", err)
	}
}

func TestResumeFollowsCrashPersistedGateOverrideTarget(t *testing.T) {
	machine := overrideFixtureMachine(t, apiv1.EvaluatorAgentic)
	runsDir, fixtureRepo, wtMgr := newTestRunnerEnv(t)
	const runID = "override-crash-recovery"
	createEscalatedGateRun(t, runsDir, runID, machine)

	jr, _, err := journal.Recover(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if err := jr.Append(journal.Event{
		Type: journal.EventGateOverridden, Gate: "review", Verdict: "needs-changes",
		Target: "fix",
		Actor:  "maintainer", Rationale: "manual inspection", Status: string(journal.PhaseEscalated),
		WorkflowVersion: machine.Def.Version, WorkflowDigest: machine.Digest(),
	}); err != nil {
		t.Fatalf("append override: %v", err)
	}
	if err := jr.Close(); err != nil {
		t.Fatalf("close interrupted override: %v", err)
	}

	det := &countingDeterministic{}
	r := terminalResumeRunner(t, runsDir, fixtureRepo, wtMgr, det)
	result, err := r.Resume(context.Background(), ResumeInput{
		RunID: runID, Machine: machine,
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if result.Phase != journal.PhaseCompleted {
		t.Fatalf("result = %+v, want completed", result)
	}
	if det.calls != 1 {
		t.Fatalf("deterministic calls = %d, want 1", det.calls)
	}
}
