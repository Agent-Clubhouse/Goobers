package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

func humanGateFixtureMachine(t *testing.T, approvers ...string) *workflow.Machine {
	t.Helper()
	machine, err := workflow.Compile(workflow.Definition{
		Name:    "human-approval",
		Version: 1,
		Spec: apiv1.WorkflowSpec{
			Gaggle: "acme-web",
			Start:  "approval",
			Gates: []apiv1.Gate{
				{
					Name:      "approval",
					Evaluator: apiv1.EvaluatorHuman,
					Human:     &apiv1.HumanGate{Approvers: approvers},
					Branches: map[string]string{
						"pass":          workflow.TerminalComplete,
						"needs-changes": "revision",
						"reject":        workflow.TargetAbort,
					},
				},
				{
					Name:      "revision",
					Evaluator: apiv1.EvaluatorHuman,
					Human:     &apiv1.HumanGate{},
					Branches: map[string]string{
						"pass":   workflow.TerminalComplete,
						"reject": workflow.TargetAbort,
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("compile human gate fixture: %v", err)
	}
	return machine
}

func startHumanGateRun(t *testing.T, r *Runner, machine *workflow.Machine, runID string) Result {
	t.Helper()
	result, err := r.Start(context.Background(), StartInput{
		RunID:   runID,
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: humanGateRepoRef(),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if result.Phase != journal.PhaseRunning || result.FinalState != "approval" {
		t.Fatalf("Start result = %+v, want running at approval", result)
	}
	return result
}

func humanGateRepoRef() apiv1.RepoRef {
	return apiv1.RepoRef{
		Provider: apiv1.ProviderGitHub,
		Owner:    "acme",
		Name:     "web",
		Branch:   "main",
	}
}

func humanResumeInput(runID string, machine *workflow.Machine, gate string, pauseSeq uint64, decision string) ResumeInput {
	return ResumeInput{
		RunID:         runID,
		Machine:       machine,
		HumanDecision: &HumanGateDecision{Gate: gate, PauseSeq: pauseSeq, Decision: decision},
		RepoRef:       humanGateRepoRef(),
	}
}

type humanGateCountingAutomated struct {
	calls int
}

func (a *humanGateCountingAutomated) Evaluate(context.Context, apiv1.AutomatedGate, apiv1.InvocationEnvelope) (string, error) {
	a.calls++
	return gate.OutcomePass, nil
}

func latestHumanPauseSeq(t *testing.T, runsDir, runID, gateName string) uint64 {
	t.Helper()
	rd, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == journal.EventGatePaused && events[i].Gate == gateName {
			return events[i].Seq
		}
	}
	t.Fatalf("run %q has no pause for gate %q", runID, gateName)
	return 0
}

func TestRunnerHumanGatePassAndReject(t *testing.T) {
	machine := humanGateFixtureMachine(t)
	r, runsDir := newTestRunnerWithDeterministic(t, nil, nil)

	tests := []struct {
		name      string
		decision  string
		wantPhase journal.RunPhase
	}{
		{name: "pass", decision: "pass", wantPhase: journal.PhaseCompleted},
		{name: "reject", decision: "reject", wantPhase: journal.PhaseAborted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runID := "human-" + tt.name
			startHumanGateRun(t, r, machine, runID)

			rd, err := journal.OpenRead(filepath.Join(runsDir, runID))
			if err != nil {
				t.Fatalf("OpenRead: %v", err)
			}
			before, err := rd.Events()
			if err != nil {
				t.Fatalf("Events before resume: %v", err)
			}
			if len(before) != 2 || before[1].Type != journal.EventGatePaused || before[1].Gate != "approval" {
				t.Fatalf("events before resume = %+v, want durable approval pause", before)
			}

			paused, err := r.Resume(context.Background(), ResumeInput{
				RunID:   runID,
				Machine: machine,
				RepoRef: humanGateRepoRef(),
			})
			if err != nil {
				t.Fatalf("crash Resume without decision: %v", err)
			}
			if paused.Phase != journal.PhaseRunning || paused.FinalState != "approval" {
				t.Fatalf("crash Resume result = %+v, want still awaiting approval", paused)
			}
			afterCrash, err := rd.Events()
			if err != nil {
				t.Fatalf("Events after crash resume: %v", err)
			}
			if len(afterCrash) != len(before) {
				t.Fatalf("crash Resume appended events: before=%d after=%d", len(before), len(afterCrash))
			}

			result, err := r.Resume(context.Background(), humanResumeInput(runID, machine, "approval", 2, tt.decision))
			if err != nil {
				t.Fatalf("Resume with %q: %v", tt.decision, err)
			}
			if result.Phase != tt.wantPhase {
				t.Fatalf("Resume phase = %q, want %q", result.Phase, tt.wantPhase)
			}

			events, err := rd.Events()
			if err != nil {
				t.Fatalf("Events after decision: %v", err)
			}
			if len(events) != 4 {
				t.Fatalf("event count = %d, want run.started, gate.paused, gate.evaluated, run.finished", len(events))
			}
			evaluated := events[2]
			if evaluated.Type != journal.EventGateEvaluated || evaluated.Gate != "approval" ||
				evaluated.Verdict != tt.decision {
				t.Fatalf("decision event = %+v, want approval gate.evaluated verdict %q", evaluated, tt.decision)
			}
			finalizations := 0
			r.cfg.FinalizeTerminal = func(gotRunID string, gotPhase journal.RunPhase) error {
				finalizations++
				if gotRunID != runID || gotPhase != tt.wantPhase {
					t.Fatalf("FinalizeTerminal(%q, %q), want (%q, %q)", gotRunID, gotPhase, runID, tt.wantPhase)
				}
				return nil
			}
			if _, err := r.Resume(context.Background(), humanResumeInput(runID, machine, "approval", 2, tt.decision)); err == nil ||
				!strings.Contains(err.Error(), "no longer awaiting a human gate decision") {
				t.Fatalf("stale terminal decision error = %v", err)
			}
			r.cfg.FinalizeTerminal = nil
			if finalizations != 1 {
				t.Fatalf("terminal decision retry finalized %d times, want 1", finalizations)
			}
		})
	}
}

func TestRunnerResumeUsesPinnedDefinitionWhenMachineOmitted(t *testing.T) {
	machine := humanGateFixtureMachine(t)
	r, runsDir := newTestRunnerWithDeterministic(t, nil, nil)
	const runID = "human-pinned-definition"
	startHumanGateRun(t, r, machine, runID)

	result, err := r.Resume(context.Background(), ResumeInput{
		RunID:   runID,
		RepoRef: humanGateRepoRef(),
		HumanDecision: &HumanGateDecision{
			Gate:     "approval",
			PauseSeq: latestHumanPauseSeq(t, runsDir, runID, "approval"),
			Decision: "pass",
		},
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if result.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", result.Phase)
	}
}

func TestRunnerHumanGateDecisionFailsClosed(t *testing.T) {
	machine := humanGateFixtureMachine(t)
	r, runsDir := newTestRunnerWithDeterministic(t, nil, nil)
	const runID = "human-fail-closed"
	startHumanGateRun(t, r, machine, runID)

	for _, test := range []struct {
		name     string
		gate     string
		decision string
		wantErr  string
	}{
		{name: "wrong gate", gate: "revision", decision: "pass", wantErr: `awaiting human gate "approval" pause 2, not "revision" pause 2`},
		{name: "unknown decision", gate: "approval", decision: "approve", wantErr: `decision "approve" has no defined branch`},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := r.Resume(context.Background(), humanResumeInput(runID, machine, test.gate, 2, test.decision)); err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Resume error = %v, want %q", err, test.wantErr)
			}
		})
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events after refused decisions: %v", err)
	}
	if len(events) != 2 || events[len(events)-1].Type != journal.EventGatePaused {
		t.Fatalf("refused decisions mutated journal: %+v", events)
	}

	result, err := r.Resume(context.Background(), humanResumeInput(runID, machine, "approval", 2, "needs-changes"))
	if err != nil {
		t.Fatalf("Resume needs-changes: %v", err)
	}
	if result.Phase != journal.PhaseRunning || result.FinalState != "revision" {
		t.Fatalf("needs-changes result = %+v, want running at revision", result)
	}
	if _, err := r.Resume(context.Background(), humanResumeInput(runID, machine, "approval", 2, "pass")); err == nil ||
		!strings.Contains(err.Error(), `awaiting human gate "revision" pause 4, not "approval" pause 2`) {
		t.Fatalf("stale decision error = %v", err)
	}

	result, err = r.Resume(context.Background(), humanResumeInput(runID, machine, "revision", 4, "pass"))
	if err != nil {
		t.Fatalf("Resume revision pass: %v", err)
	}
	if result.Phase != journal.PhaseCompleted {
		t.Fatalf("revision pass phase = %q, want completed", result.Phase)
	}
}

func TestRunnerHumanGateEnforcesConfiguredApprovers(t *testing.T) {
	machine := humanGateFixtureMachine(t, "maintainers")
	r, runsDir := newTestRunnerWithDeterministic(t, nil, nil)
	const runID = "human-restricted-approvers"
	startHumanGateRun(t, r, machine, runID)

	pauseSeq := latestHumanPauseSeq(t, runsDir, runID, "approval")
	unauthorized := humanResumeInput(runID, machine, "approval", pauseSeq, "pass")
	unauthorized.HumanDecision.Actor = "contributor"
	if _, err := r.Resume(context.Background(), unauthorized); err == nil ||
		!strings.Contains(err.Error(), `actor "contributor" is not an authorized approver`) {
		t.Fatalf("unauthorized Resume error = %v, want authorization failure", err)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events after refused decision: %v", err)
	}
	if len(events) != 2 || events[len(events)-1].Type != journal.EventGatePaused {
		t.Fatalf("unauthorized decision mutated journal: %+v", events)
	}

	authorized := humanResumeInput(runID, machine, "approval", pauseSeq, "pass")
	authorized.HumanDecision.Actor = "maintainers"
	result, err := r.Resume(context.Background(), authorized)
	if err != nil {
		t.Fatalf("authorized Resume: %v", err)
	}
	if result.Phase != journal.PhaseCompleted {
		t.Fatalf("authorized Resume phase = %q, want completed", result.Phase)
	}
	events, err = rd.Events()
	if err != nil {
		t.Fatalf("Events after authorized decision: %v", err)
	}
	if len(events) != 4 || events[2].Type != journal.EventGateEvaluated || events[2].Actor != "maintainers" {
		t.Fatalf("authorized decision events = %+v, want actor on gate.evaluated", events)
	}
}

func TestRunnerHumanGateReplaysRecordedDecision(t *testing.T) {
	machine := humanGateFixtureMachine(t)
	r, runsDir := newTestRunnerWithDeterministic(t, nil, nil)

	tests := []struct {
		name          string
		decision      string
		wantPhase     journal.RunPhase
		retryDecision bool
		removeState   bool
	}{
		{name: "crash resume", decision: "pass", wantPhase: journal.PhaseCompleted, removeState: true},
		{name: "idempotent submit", decision: "reject", wantPhase: journal.PhaseAborted, retryDecision: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runID := "human-recorded-" + strings.ReplaceAll(test.name, " ", "-")
			startHumanGateRun(t, r, machine, runID)

			run, _, err := journal.Recover(filepath.Join(runsDir, runID))
			if err != nil {
				t.Fatalf("Recover: %v", err)
			}
			approval, ok := machine.Gate("approval")
			if !ok {
				t.Fatal("fixture has no approval gate")
			}
			if _, err := (&gate.Evaluator{Journal: run}).EvaluateHuman(approval, test.decision, ""); err != nil {
				t.Fatalf("record human decision: %v", err)
			}
			if err := run.Close(); err != nil {
				t.Fatalf("close interrupted run: %v", err)
			}
			if test.removeState {
				if err := os.Remove(filepath.Join(runsDir, runID, "state.json")); err != nil {
					t.Fatalf("remove state checkpoint: %v", err)
				}
			}

			input := ResumeInput{RunID: runID, Machine: machine, RepoRef: humanGateRepoRef()}
			if test.retryDecision {
				input.HumanDecision = &HumanGateDecision{Gate: "approval", PauseSeq: 2, Decision: test.decision}
			}
			result, err := r.Resume(context.Background(), input)
			if err != nil {
				t.Fatalf("Resume recorded decision: %v", err)
			}
			if result.Phase != test.wantPhase {
				t.Fatalf("Resume phase = %q, want %q", result.Phase, test.wantPhase)
			}

			rd, err := journal.OpenRead(filepath.Join(runsDir, runID))
			if err != nil {
				t.Fatalf("OpenRead: %v", err)
			}
			events, err := rd.Events()
			if err != nil {
				t.Fatalf("Events: %v", err)
			}
			want := []journal.EventType{
				journal.EventRunStarted,
				journal.EventGatePaused,
				journal.EventGateEvaluated,
				journal.EventRunFinished,
			}
			var got []journal.EventType
			for _, event := range events {
				got = append(got, event.Type)
			}
			if !eventTypesEqual(got, want) {
				t.Fatalf("event sequence = %v, want %v", got, want)
			}
			if events[2].Verdict != test.decision {
				t.Fatalf("replayed verdict = %q, want %q", events[2].Verdict, test.decision)
			}
		})
	}
}

func TestRunnerHumanGateDecisionIsBoundToPauseOccurrence(t *testing.T) {
	machine, err := workflow.Compile(workflow.Definition{
		Name:    "repeated-human-approval",
		Version: 1,
		Spec: apiv1.WorkflowSpec{
			Gaggle: "acme-web",
			Start:  "approval",
			Gates: []apiv1.Gate{{
				Name:      "approval",
				Evaluator: apiv1.EvaluatorHuman,
				Human:     &apiv1.HumanGate{},
				Branches: map[string]string{
					"pass":          workflow.TerminalComplete,
					"needs-changes": "approval",
				},
			}},
		},
	})
	if err != nil {
		t.Fatalf("compile repeated human gate: %v", err)
	}
	r, _ := newTestRunnerWithDeterministic(t, nil, nil)
	const runID = "human-repeated-pause"
	startHumanGateRun(t, r, machine, runID)

	result, err := r.Resume(context.Background(), humanResumeInput(runID, machine, "approval", 2, "needs-changes"))
	if err != nil {
		t.Fatalf("Resume needs-changes: %v", err)
	}
	if result.Phase != journal.PhaseRunning || result.FinalState != "approval" {
		t.Fatalf("needs-changes result = %+v, want a new approval pause", result)
	}

	if _, err := r.Resume(context.Background(), humanResumeInput(runID, machine, "approval", 2, "pass")); err == nil ||
		!strings.Contains(err.Error(), `awaiting human gate "approval" pause 4, not "approval" pause 2`) {
		t.Fatalf("prior-pause decision error = %v", err)
	}
	result, err = r.Resume(context.Background(), humanResumeInput(runID, machine, "approval", 4, "pass"))
	if err != nil {
		t.Fatalf("Resume current pause: %v", err)
	}
	if result.Phase != journal.PhaseCompleted {
		t.Fatalf("current-pause phase = %q, want completed", result.Phase)
	}
}

func TestRunnerHumanGateRecordedDecisionOverridesMissingCheckpoint(t *testing.T) {
	machine, err := workflow.Compile(workflow.Definition{
		Name:    "post-task-human-approval",
		Version: 1,
		Spec: apiv1.WorkflowSpec{
			Gaggle: "acme-web",
			Start:  "prepare",
			Tasks: []apiv1.Task{{
				Name: "prepare",
				Type: apiv1.TaskDeterministic,
				Goal: "prepare approval",
				Run:  &apiv1.DeterministicRun{Command: []string{"true"}},
				Next: "approval",
			}},
			Gates: []apiv1.Gate{{
				Name:      "approval",
				Evaluator: apiv1.EvaluatorHuman,
				Human:     &apiv1.HumanGate{},
				Branches:  map[string]string{"pass": workflow.TerminalComplete},
			}},
		},
	})
	if err != nil {
		t.Fatalf("compile post-task human gate: %v", err)
	}
	deterministic := &stubDeterministic{byTask: map[string]stubTaskResult{
		"post-task-human:prepare": {status: apiv1.ResultSuccess},
	}}
	r, runsDir := newTestRunnerWithDeterministic(t, func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
		return deterministic, nil
	}, nil)
	const runID = "post-task-human"
	startHumanGateRun(t, r, machine, runID)

	run, _, err := journal.Recover(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	approval, _ := machine.Gate("approval")
	if _, err := (&gate.Evaluator{Journal: run}).EvaluateHuman(approval, "pass", ""); err != nil {
		t.Fatalf("record human decision: %v", err)
	}
	if err := run.Close(); err != nil {
		t.Fatalf("close interrupted run: %v", err)
	}
	if err := os.Remove(filepath.Join(runsDir, runID, "state.json")); err != nil {
		t.Fatalf("remove state checkpoint: %v", err)
	}

	result, err := r.Resume(context.Background(), ResumeInput{
		RunID: runID, Machine: machine, RepoRef: humanGateRepoRef(),
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if result.Phase != journal.PhaseCompleted {
		t.Fatalf("Resume phase = %q, want completed", result.Phase)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var stageStarts, pauses, verdicts int
	for _, event := range events {
		switch event.Type {
		case journal.EventStageStarted:
			stageStarts++
		case journal.EventGatePaused:
			pauses++
		case journal.EventGateEvaluated:
			verdicts++
		}
	}
	if stageStarts != 1 || pauses != 1 || verdicts != 1 {
		t.Fatalf("replay counts: stage.started=%d gate.paused=%d gate.evaluated=%d, want 1 each", stageStarts, pauses, verdicts)
	}
}

func TestRunnerHumanGateDecisionOverridesStalePredecessorCheckpoint(t *testing.T) {
	machine, err := workflow.Compile(workflow.Definition{
		Name:    "prechecked-human-approval",
		Version: 1,
		Spec: apiv1.WorkflowSpec{
			Gaggle: "acme-web",
			Start:  "precheck",
			Gates: []apiv1.Gate{
				{
					Name:      "precheck",
					Evaluator: apiv1.EvaluatorAutomated,
					Automated: &apiv1.AutomatedGate{Check: "status-equals"},
					Branches:  map[string]string{gate.OutcomePass: "approval", gate.OutcomeFail: workflow.TargetAbort},
				},
				{
					Name:      "approval",
					Evaluator: apiv1.EvaluatorHuman,
					Human:     &apiv1.HumanGate{},
					Branches:  map[string]string{"pass": workflow.TerminalComplete},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("compile prechecked human gate: %v", err)
	}
	automated := &humanGateCountingAutomated{}
	r, runsDir := newTestRunnerWithDeterministic(t, nil, automated)
	const runID = "human-stale-predecessor-checkpoint"
	startHumanGateRun(t, r, machine, runID)
	if automated.calls != 1 {
		t.Fatalf("precheck calls after start = %d, want 1", automated.calls)
	}

	run, _, err := journal.Recover(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	run.SetMachineState("precheck")
	if err := run.Checkpoint(); err != nil {
		t.Fatalf("write stale checkpoint: %v", err)
	}
	if err := run.Close(); err != nil {
		t.Fatalf("close interrupted run: %v", err)
	}

	result, err := r.Resume(context.Background(), humanResumeInput(
		runID, machine, "approval", latestHumanPauseSeq(t, runsDir, runID, "approval"), "pass",
	))
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if result.Phase != journal.PhaseCompleted {
		t.Fatalf("Resume phase = %q, want completed", result.Phase)
	}
	if automated.calls != 1 {
		t.Fatalf("precheck calls after resume = %d, want journaled waiting gate to prevent re-evaluation", automated.calls)
	}
}

func TestRunnerHumanGateExplicitCompletionClearsStageFailure(t *testing.T) {
	machine, err := workflow.Compile(workflow.Definition{
		Name:    "human-clears-failure",
		Version: 1,
		Spec: apiv1.WorkflowSpec{
			Gaggle: "acme-web",
			Start:  "implement",
			Tasks: []apiv1.Task{{
				Name: "implement",
				Type: apiv1.TaskDeterministic,
				Goal: "attempt implementation",
				Run:  &apiv1.DeterministicRun{Command: []string{"true"}},
				Next: "approval",
			}},
			Gates: []apiv1.Gate{{
				Name:      "approval",
				Evaluator: apiv1.EvaluatorHuman,
				Human:     &apiv1.HumanGate{},
				Branches: map[string]string{
					"approved": workflow.TerminalComplete,
					"rejected": workflow.TargetAbort,
				},
			}},
		},
	})
	if err != nil {
		t.Fatalf("compile human failure override: %v", err)
	}
	const runID = "human-clears-failure"
	r, runsDir := newTestRunner(t, map[string]stubTaskResult{
		runID + ":implement": {
			status:    apiv1.ResultFailure,
			errorInfo: &apiv1.ErrorInfo{Code: "implementation_failed", Message: "requires human disposition"},
		},
	}, nil)
	startHumanGateRun(t, r, machine, runID)

	result, err := r.Resume(context.Background(), humanResumeInput(
		runID, machine, "approval", latestHumanPauseSeq(t, runsDir, runID, "approval"), "approved",
	))
	if err != nil {
		t.Fatalf("Resume approved: %v", err)
	}
	if result.Phase != journal.PhaseCompleted {
		t.Fatalf("approved phase = %q, want completed from the declared human branch", result.Phase)
	}
}
