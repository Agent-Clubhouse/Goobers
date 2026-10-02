package engine

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"github.com/goobers/goobers/internal/temporaltest"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
)

func TestAgenticUsageBudgetUnavailableAfterTimeout(t *testing.T) {
	for _, tc := range []struct {
		name      string
		firstErr  error
		wantCalls int
	}{
		{"start-to-close fails closed", temporal.NewTimeoutError(enumspb.TIMEOUT_TYPE_START_TO_CLOSE, nil), 1},
		{"heartbeat fails closed", temporal.NewTimeoutError(enumspb.TIMEOUT_TYPE_HEARTBEAT, nil), 1},
		{"schedule-to-start retries", temporal.NewTimeoutError(enumspb.TIMEOUT_TYPE_SCHEDULE_TO_START, nil), 2},
		{"provisioning retries", classifySeamError(invoke.InfrastructureFailure(errors.New("workspace unavailable"))), 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var suite testsuite.WorkflowTestSuite
			env := temporaltest.NewWorkflowEnvironment(&suite)
			var calls int
			var rec runJournal
			env.ExecuteWorkflow(func(ctx workflow.Context) (apiv1.ResultEnvelope, error) {
				task := agenticRetrySpec(&apiv1.RetryPolicy{MaxAttempts: 3}).Tasks[0]
				task.Limits = &apiv1.Limits{MaxTokens: 10, MaxCostUSD: 1}
				return dispatchWithRetry(ctx, RunInput{}, task, &rec, nil, func(workflow.Context, int, journal.AttemptClass) (stageActivityResult, error) {
					calls++
					if calls == 1 {
						return stageActivityResult{}, tc.firstErr
					}
					return stageActivityResult{
						ResultEnvelope: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess},
						Usage:          &AttemptUsage{Reported: true, Metrics: usageMetrics(5, 0.5)},
					}, nil
				}, nil)
			})
			if err := env.GetWorkflowError(); err != nil {
				t.Fatal(err)
			}
			if calls != tc.wantCalls {
				t.Fatalf("dispatches=%d want=%d", calls, tc.wantCalls)
			}
			var result apiv1.ResultEnvelope
			if err := env.GetWorkflowResult(&result); err != nil {
				t.Fatal(err)
			}
			if tc.wantCalls == 2 {
				if result.Status != apiv1.ResultSuccess {
					t.Fatalf("safe retry result=%+v", result)
				}
				return
			}
			if result.Status != apiv1.ResultFailure || result.Error == nil || result.Error.Code != runner.BudgetExceededErrorCode || result.Error.Retryable {
				t.Fatalf("worker-loss result=%+v", result)
			}
			for _, op := range rec.proj.Ops {
				if op.Event != nil && op.Event.Type == journal.EventStageFinished && op.Event.Attempt == 1 && op.Event.Error != nil && op.Event.Error.Code == runner.BudgetExceededErrorCode {
					return
				}
			}
			t.Fatal("worker loss did not journal the terminal budget failure")
		})
	}
}

func TestAgenticUsageBudgets(t *testing.T) {
	for _, tc := range []struct {
		name        string
		limits      *apiv1.Limits
		usage       map[string]float64
		wantFailure bool
	}{
		{"token boundary", &apiv1.Limits{MaxTokens: 10}, usageMetrics(5, 0), false},
		{"decimal cost boundary", &apiv1.Limits{MaxCostUSD: 0.2}, usageMetrics(0, 0.1), false},
		{"unconfigured", nil, nil, false},
		{"token exceeded", &apiv1.Limits{MaxTokens: 9}, usageMetrics(5, 0), true},
		{"cost exceeded", &apiv1.Limits{MaxCostUSD: 0.19}, usageMetrics(0, 0.1), true},
		{"missing", &apiv1.Limits{MaxTokens: 10}, map[string]float64{}, true},
		{"unreported successful attempt", &apiv1.Limits{MaxTokens: 10}, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := func() JournalProjection {
				calls := 0
				g := &fakeInvoker{invoke: func(ctx context.Context, _ apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
					calls++
					if tc.usage != nil {
						invoke.ReportAgentUsage(ctx, tc.usage)
					}
					if calls == 1 {
						return apiv1.ResultEnvelope{}, errors.New("adapter failed")
					}
					return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
				}}
				spec := agenticRetrySpec(&apiv1.RetryPolicy{MaxAttempts: 3})
				spec.Tasks[0].Limits = tc.limits
				return executeForProjection(t, runInput("usage-budget", spec), &Activities{Goober: g, Workspaces: testWorkspaces(t)}, false)
			}
			first := run()
			found := false
			for _, op := range first.Ops {
				if op.Event != nil && op.Event.Type == journal.EventStageFinished && op.Event.Error != nil && op.Event.Error.Code == "budget-exceeded" {
					found = true
				}
			}
			if found != tc.wantFailure {
				t.Fatalf("budget failure=%v want=%v", found, tc.wantFailure)
			}
			if second := run(); !reflect.DeepEqual(first, second) {
				t.Fatal("identical history inputs produced different projections")
			}
		})
	}
}
func TestFailedAgenticUsagePreservesInfrastructureRetry(t *testing.T) {
	retryAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	original := classifySeamError(invoke.InfrastructureFailureUntil(errors.New("provider unavailable"), retryAt))
	err := agenticUsageError(original, AttemptUsage{Reported: true, Metrics: usageMetrics(6, 0.2)})
	// Serialize through the same failure converter Temporal uses.
	converter := temporal.GetDefaultFailureConverter()
	err = converter.FailureToError(converter.ErrorToFailure(err))
	if got := dispatchFailureUsage(err); !got.Reported || got.Metrics == nil {
		t.Fatalf("lost usage: %+v", got)
	}
	if got := infrastructureRetryDelay(err, 0, retryAt.Add(-time.Minute)); got != time.Minute {
		t.Fatalf("retry delay=%v", got)
	}
}
func TestBudgetRefusalDependsOnAgenticPlacement(t *testing.T) {
	spec := agenticRetrySpec(nil)
	spec.Tasks[0].Limits = &apiv1.Limits{MaxTokens: 10}
	if err := RefusePlacedDefinition("flow", spec, nil); err != nil {
		t.Fatal(err)
	}
	if err := RefusePlacedDefinition("flow", spec, []PinnedPlacement{{Stage: "implement", Self: true}}); err != nil {
		t.Fatal(err)
	}
	if err := RefusePlacedDefinition("flow", spec, []PinnedPlacement{remotePin("implement")}); !errors.Is(err, ErrRemoteUsageLimitsUnsupported) {
		t.Fatalf("remote refusal=%v", err)
	}
}

// Histories admitted before enforcement retain their original routing even
// when their successful activity result did not carry the new usage field.
func TestAgenticUsageBudgetLegacyVersion(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&suite)
	env.OnGetVersion("cumulative-agentic-usage", workflow.DefaultVersion, 1).Return(workflow.DefaultVersion)
	env.RegisterActivity(&Activities{Goober: &fakeInvoker{invoke: func(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
	}}, Workspaces: testWorkspaces(t)})
	spec := agenticRetrySpec(nil)
	spec.Tasks[0].Limits = &apiv1.Limits{MaxTokens: 10}
	env.ExecuteWorkflow(Run, runInput("legacy-budget", spec))
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var result RunResult
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusCompleted {
		t.Fatalf("legacy status=%s", result.Status)
	}
}
