package engine

import (
	"context"
	"testing"

	"go.temporal.io/sdk/testsuite"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/temporaltest"
)

// #5182 engine parity: an UNCOMMITTED_CHANGES failure spends the stage's
// policy retry exactly as the local runner does.
func TestAgenticUncommittedChangesConsumesPolicyRetry(t *testing.T) {
	var calls int
	goober := &fakeInvoker{invoke: func(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
		calls++
		if calls == 1 {
			return apiv1.ResultEnvelope{
				Status: apiv1.ResultFailure,
				Error: &apiv1.ErrorInfo{
					Code:      runner.UncommittedChangesCode,
					Message:   "UNCOMMITTED_CHANGES: the stage reported success but left its changes uncommitted",
					Retryable: true,
				},
			}, nil
		}
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "committed"}, nil
	}}
	var ts testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&ts)
	env.RegisterActivity(&Activities{Goober: goober, Workspaces: testWorkspaces(t)})
	env.ExecuteWorkflow(Run, runInput("agentic-uncommitted-retry", agenticRetrySpec(&apiv1.RetryPolicy{MaxAttempts: 2})))

	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	if calls != 2 {
		t.Fatalf("agentic dispatches = %d, want 2 (uncommitted retry plus success)", calls)
	}
}
