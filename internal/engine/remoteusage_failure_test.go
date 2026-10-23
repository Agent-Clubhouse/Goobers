package engine

import (
	"context"
	"errors"
	"testing"

	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
	corev1 "k8s.io/api/core/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/temporaltest"
	"github.com/goobers/goobers/internal/usageallowance"
)

func TestConfirmedExecutionWithUnreadableUsageFailsClosed(t *testing.T) {
	for _, scenario := range []string{"missing", "unreadable", "invalid"} {
		t.Run(scenario, func(t *testing.T) {
			store := surrenderStore(t)
			switch scenario {
			case "unreadable":
				if err := store.Put(t.Context(), "lost-usage", "build", 1, []byte("{")); err != nil {
					t.Fatal(err)
				}
			case "invalid":
				if err := store.Put(t.Context(), "lost-usage", "build", 1, []byte(`{"contractVersion":99}`)); err != nil {
					t.Fatal(err)
				}
			}
			putSurrendered(t, store, "lost-usage", "build", 2, dispatcher.SurrenderedResult{
				ContractVersion: dispatcher.SurrenderContractV2,
				ObservedUsage:   usageMetrics(7, 0), ObservedUsageReported: true,
				Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess},
			})
			fake := &fakeStageDispatcher{report: dispatcher.Report{
				Runner: "linux", Pod: "spent-pod", Phase: corev1.PodSucceeded,
				SurrenderConfirmed: true, Disposed: true,
			}}
			var allowances []int64
			result := runRemoteUsageFailureWorkflow(t, &Activities{Dispatcher: fake, Surrenders: store}, func(ctx workflow.Context, attempt int, allowance *usageallowance.Allowance) (stageActivityResult, error) {
				allowances = append(allowances, *allowance.MaxTokens)
				input := dispatchInput("lost-usage", "build", attempt)
				input.Envelope.Limits.MaxTokens = 10
				input.UsageAllowance = versionedRemoteUsageAllowance(ctx, allowance)
				return executeDispatchActivity(dispatchActivityContext(ctx, input.Envelope.Limits, ""), input)
			})
			if len(allowances) != 1 || fake.calls.Load() != 1 {
				t.Fatalf("allowances=%v dispatches=%d, want one spent attempt and no retry", allowances, fake.calls.Load())
			}
			if result.Error == nil || result.Error.Code != runner.BudgetExceededErrorCode {
				t.Fatalf("result=%+v, want fail-closed budget result", result)
			}
		})
	}
}

func TestPostExecutionFailureChargesKnownUsage(t *testing.T) {
	store := surrenderStore(t)
	putSurrendered(t, store, "known-usage", "build", 1, dispatcher.SurrenderedResult{
		ContractVersion: dispatcher.SurrenderContractV2,
		ObservedUsage:   usageMetrics(7, 0), ObservedUsageReported: true,
		Result:  apiv1.ResultEnvelope{Status: apiv1.ResultSuccess},
		Verdict: &apiv1.Verdict{Decision: apiv1.VerdictPass},
	})
	putSurrendered(t, store, "known-usage", "build", 2, dispatcher.SurrenderedResult{
		ContractVersion: dispatcher.SurrenderContractV2,
		ObservedUsage:   usageMetrics(2, 0), ObservedUsageReported: true,
		Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess},
	})
	fake := &fakeStageDispatcher{report: dispatcher.Report{
		Runner: "linux", Pod: "usage-pod", Phase: corev1.PodSucceeded,
		SurrenderConfirmed: true, Disposed: true,
	}}
	var allowances []int64
	result := runRemoteUsageFailureWorkflow(t, &Activities{Dispatcher: fake, Surrenders: store}, func(ctx workflow.Context, attempt int, allowance *usageallowance.Allowance) (stageActivityResult, error) {
		allowances = append(allowances, *allowance.MaxTokens)
		input := dispatchInput("known-usage", "build", attempt)
		input.Envelope.Limits.MaxTokens = 10
		input.UsageAllowance = versionedRemoteUsageAllowance(ctx, allowance)
		return executeDispatchActivity(dispatchActivityContext(ctx, input.Envelope.Limits, ""), input)
	})
	if result.Status != apiv1.ResultSuccess || result.Error != nil {
		t.Fatalf("result=%+v, want successful retry", result)
	}
	if len(allowances) != 2 || allowances[0] != 10 || allowances[1] != 3 {
		t.Fatalf("allowances=%v, want [10 3]", allowances)
	}
}

func TestPostExecutionFailureWithEmptyUsageFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name    string
		version int
	}{
		{"v1", dispatcher.SurrenderContractV1},
		{"v2", dispatcher.SurrenderContractV2},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := surrenderStore(t)
			putSurrendered(t, store, "empty-usage", "build", 1, dispatcher.SurrenderedResult{
				ContractVersion: test.version,
				Result:          apiv1.ResultEnvelope{Status: apiv1.ResultSuccess},
				Verdict:         &apiv1.Verdict{Decision: apiv1.VerdictPass},
			})
			putSurrendered(t, store, "empty-usage", "build", 2, dispatcher.SurrenderedResult{
				ContractVersion: dispatcher.SurrenderContractV2,
				ObservedUsage:   usageMetrics(7, 0), ObservedUsageReported: true,
				Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess},
			})
			fake := &fakeStageDispatcher{report: dispatcher.Report{
				Runner: "linux", Pod: "spent-pod", Phase: corev1.PodSucceeded,
				SurrenderConfirmed: true, Disposed: true,
			}}
			var allowances []int64
			result := runRemoteUsageFailureWorkflow(t, &Activities{Dispatcher: fake, Surrenders: store}, func(ctx workflow.Context, attempt int, allowance *usageallowance.Allowance) (stageActivityResult, error) {
				allowances = append(allowances, *allowance.MaxTokens)
				input := dispatchInput("empty-usage", "build", attempt)
				input.Envelope.Limits.MaxTokens = 10
				input.UsageAllowance = versionedRemoteUsageAllowance(ctx, allowance)
				return executeDispatchActivity(dispatchActivityContext(ctx, input.Envelope.Limits, ""), input)
			})
			if len(allowances) != 1 || fake.calls.Load() != 1 {
				t.Fatalf("allowances=%v dispatches=%d, want no free retry", allowances, fake.calls.Load())
			}
			if result.Error == nil || result.Error.Code != runner.BudgetExceededErrorCode {
				t.Fatalf("result=%+v, want fail-closed budget result", result)
			}
		})
	}
}

type preExecutionRetryDispatcher struct {
	calls int
}

func (d *preExecutionRetryDispatcher) Dispatch(_ context.Context, attempt dispatcher.Attempt, _ []dispatcher.RunnerSpec) (dispatcher.Report, error) {
	d.calls++
	if d.calls == 1 {
		return dispatcher.Report{Runner: "linux"}, invoke.InfrastructureFailure(errors.New("capacity unavailable"))
	}
	return dispatcher.Report{Runner: "linux", Pod: "retry-pod", Phase: corev1.PodSucceeded, SurrenderConfirmed: true}, nil
}

func TestPreExecutionFailureRetainsFullAllowance(t *testing.T) {
	store := surrenderStore(t)
	putSurrendered(t, store, "pre-execution", "build", 2, dispatcher.SurrenderedResult{
		ContractVersion: dispatcher.SurrenderContractV2,
		ObservedUsage:   usageMetrics(2, 0), ObservedUsageReported: true,
		Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess},
	})
	fake := &preExecutionRetryDispatcher{}
	var allowances []int64
	result := runRemoteUsageFailureWorkflow(t, &Activities{Dispatcher: fake, Surrenders: store}, func(ctx workflow.Context, attempt int, allowance *usageallowance.Allowance) (stageActivityResult, error) {
		allowances = append(allowances, *allowance.MaxTokens)
		input := dispatchInput("pre-execution", "build", attempt)
		input.Envelope.Limits.MaxTokens = 10
		input.UsageAllowance = versionedRemoteUsageAllowance(ctx, allowance)
		return executeDispatchActivity(dispatchActivityContext(ctx, input.Envelope.Limits, ""), input)
	})
	if result.Status != apiv1.ResultSuccess || fake.calls != 2 {
		t.Fatalf("result=%+v dispatches=%d, want successful pre-execution retry", result, fake.calls)
	}
	if len(allowances) != 2 || allowances[0] != 10 || allowances[1] != 10 {
		t.Fatalf("allowances=%v, want [10 10]", allowances)
	}
}

type startedFailureDispatcher struct {
	calls  int
	report dispatcher.Report
}

func (d *startedFailureDispatcher) Dispatch(_ context.Context, _ dispatcher.Attempt, _ []dispatcher.RunnerSpec) (dispatcher.Report, error) {
	d.calls++
	return d.report, invoke.InfrastructureFailure(errors.New("supervision lost after create"))
}

func TestStartedExecutionWithUnknownUsageFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name   string
		report dispatcher.Report
	}{
		{"running", dispatcher.Report{Runner: "linux", Pod: "started-pod", PodCreateAttempted: true, Phase: corev1.PodRunning}},
		{"created with empty phase", dispatcher.Report{Runner: "linux", Pod: "created-pod", PodCreateAttempted: true}},
		{"ambiguous create", dispatcher.Report{Runner: "linux", PodCreateAttempted: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := &startedFailureDispatcher{report: test.report}
			var allowances []int64
			result := runRemoteUsageFailureWorkflow(t, &Activities{Dispatcher: fake, Surrenders: surrenderStore(t)}, func(ctx workflow.Context, attempt int, allowance *usageallowance.Allowance) (stageActivityResult, error) {
				allowances = append(allowances, *allowance.MaxTokens)
				input := dispatchInput("started-usage", "build", attempt)
				input.Envelope.Limits.MaxTokens = 10
				input.UsageAllowance = versionedRemoteUsageAllowance(ctx, allowance)
				return executeDispatchActivity(dispatchActivityContext(ctx, input.Envelope.Limits, ""), input)
			})
			if len(allowances) != 1 || fake.calls != 1 {
				t.Fatalf("allowances=%v dispatches=%d, want no retry after possible execution", allowances, fake.calls)
			}
			if result.Error == nil || result.Error.Code != runner.BudgetExceededErrorCode {
				t.Fatalf("result=%+v, want fail-closed budget result", result)
			}
		})
	}
}

func runRemoteUsageFailureWorkflow(
	t *testing.T,
	activities *Activities,
	dispatch func(workflow.Context, int, *usageallowance.Allowance) (stageActivityResult, error),
) apiv1.ResultEnvelope {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&suite)
	env.RegisterActivity(activities)
	env.ExecuteWorkflow(func(ctx workflow.Context) (apiv1.ResultEnvelope, error) {
		task := apiv1.Task{
			Name: "build", Type: apiv1.TaskAgentic,
			Limits: &apiv1.Limits{MaxTokens: 10},
			Retry:  &apiv1.RetryPolicy{MaxAttempts: 2},
		}
		return dispatchWithRetry(ctx, RunInput{}, task, &runJournal{}, nil,
			func(ctx workflow.Context, attempt int, _ journal.AttemptClass, allowance *usageallowance.Allowance) (stageActivityResult, error) {
				return dispatch(ctx, attempt, allowance)
			}, nil)
	})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var result apiv1.ResultEnvelope
	if err := env.GetWorkflowResult(&result); err != nil {
		t.Fatal(err)
	}
	return result
}
