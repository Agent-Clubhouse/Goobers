package engine

import (
	"context"
	"testing"

	"go.temporal.io/sdk/testsuite"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/runsemantics"
	"github.com/goobers/goobers/internal/temporaltest"
)

// #5871 engine parity: a premature execution-window claim spends the stage's
// policy retry exactly as the local runner does.
func TestAgenticPrematureExhaustionClaimConsumesPolicyRetry(t *testing.T) {
	var calls int
	goober := &fakeInvoker{invoke: func(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
		calls++
		if calls == 1 {
			return apiv1.ResultEnvelope{
				Status: apiv1.ResultFailure,
				Error: &apiv1.ErrorInfo{
					Code:      runsemantics.PrematureExhaustionClaimCode,
					Message:   "the agent reported EXECUTION_WINDOW_EXHAUSTED after 22s of its 30m0s execution window",
					Retryable: true,
				},
			}, nil
		}
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "curated"}, nil
	}}
	var ts testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&ts)
	env.RegisterActivity(&Activities{Goober: goober, Workspaces: testWorkspaces(t)})
	env.ExecuteWorkflow(Run, runInput("agentic-premature-exhaustion-retry", agenticRetrySpec(&apiv1.RetryPolicy{MaxAttempts: 2})))

	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	if calls != 2 {
		t.Fatalf("agentic dispatches = %d, want 2 (premature-claim retry plus success)", calls)
	}
}
