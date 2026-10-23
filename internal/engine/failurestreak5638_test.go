package engine

import (
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/testsuite"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/runsemantics"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/temporaltest"
	"github.com/goobers/goobers/internal/workflow"
)

// TestEngineCancelDuringGateInfraRetry is #5638's second reproduction on the
// Temporal driver. local-ci times out, the failure-class local-gate judges it
// infra and routes back to local-ci, and the stall sweep then stops the run
// with CancelWorkflow while that retry is in flight. The engine can only
// journal the cancellation as an operator-abort run_canceled terminal; the
// journal it writes must still let the failure streak see an infrastructure
// retry once the sweep's own record supplies the interruption — and never
// when no such record exists (an operator's cancel).
func TestEngineCancelDuringGateInfraRetry(t *testing.T) {
	in := runInput("local-gate-5638", apiv1.WorkflowSpec{
		Gaggle: "web", Start: "local-ci",
		Tasks: []apiv1.Task{{
			Name: "local-ci", Type: apiv1.TaskDeterministic, Goal: "run the local validation suite",
			Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}, Next: "local-gate",
		}},
		Gates: []apiv1.Gate{{
			Name: "local-gate", Evaluator: apiv1.EvaluatorAutomated, Automated: &apiv1.AutomatedGate{Check: "failure-class"},
			Branches: map[string]string{gate.OutcomePass: workflow.TerminalComplete, gate.OutcomeInfra: "local-ci", gate.OutcomeFail: workflow.TargetEscalate},
		}},
	})
	var suite testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&suite)
	env.RegisterActivity(&Activities{Auto: gate.NewAutomatedEvaluator(), Workspaces: testWorkspaces(t)})
	timedOut := stageActivityResult{ResultEnvelope: apiv1.ResultEnvelope{Status: apiv1.ResultFailure, Error: &apiv1.ErrorInfo{
		Code: telemetry.ErrCodeTimeout, Message: "local-ci exceeded its wall-clock limit", Retryable: true,
	}}}
	env.OnActivity(ActRunDeterministic, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(timedOut, nil).Once()
	// The retried attempt is still running when the sweep cancels the run.
	env.OnActivity(ActRunDeterministic, mock.Anything, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
		After(time.Hour).Return(stageActivityResult{ResultEnvelope: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}}, nil).Maybe()
	env.RegisterDelayedCallback(env.CancelWorkflow, 30*time.Minute)

	env.ExecuteWorkflow(Run, in)
	if !env.IsWorkflowCompleted() {
		t.Fatal("the cancelled workflow did not close")
	}
	events := hitlEvents(hitlProjection(t, env))
	var decision *journal.Event
	var finished *journal.Event
	for i := range events {
		switch {
		case events[i].Type == journal.EventRunnerAnnotation && events[i].Runner["kind"] == runsemantics.RetryDecisionKind:
			decision = &events[i]
		case events[i].Type == journal.EventRunFinished:
			finished = &events[i]
		}
	}
	if decision == nil || decision.Runner[runsemantics.RetryFailureClassKey] != string(journal.AttemptInfra) {
		t.Fatalf("precondition: want local-gate's infra retry decision, events = %v", hitlEventTypes(events))
	}
	if finished == nil || finished.TerminalCause == nil || finished.TerminalCause.Code != runsemantics.RunCanceledErrorCode ||
		finished.TerminalCause.Phase != journal.PhaseAborted {
		t.Fatalf("precondition: want the engine's run_canceled aborted terminal, got %+v", finished)
	}

	for interruption, want := range map[string]runner.TerminalFault{
		runsemantics.RunDurationExceededErrorCode: runner.TerminalFaultItemInfrastructure,
		runsemantics.RunStalledErrorCode:          runner.TerminalFaultItemInfrastructure,
		"":                                        runner.TerminalFaultUnclassified, // an operator's cancel
	} {
		if got := runner.JournaledTerminalFault(events, interruption); got != want {
			t.Errorf("interruption %q: class = %q, want %q", interruption, got, want)
		}
	}
}
