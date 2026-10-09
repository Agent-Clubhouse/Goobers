package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/temporaltest"
)

// #6750: an activity whose context ends without a server cancellation request
// (worker shutdown, failed heartbeat RPC) lost its control path. It must not
// surface as a bare context.Canceled, which the SDK records as an ordinary
// policy failure; a requested cancellation still does.
func TestUnconfirmedDispatchFailureSeparatesLostControlPathFromCancellation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cause     error
		wantClass journal.AttemptClass
	}{
		{name: "server cancellation", cause: temporal.NewCanceledError()},
		{name: "worker shutdown", cause: errors.New("worker is now shutdown"), wantClass: journal.AttemptInfra},
		{name: "heartbeat failure", cause: errors.New("rpc error: code = Unavailable"), wantClass: journal.AttemptInfra},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got error
			var suite testsuite.WorkflowTestSuite
			env := suite.NewTestActivityEnvironment()
			env.RegisterActivityWithOptions(func(ctx context.Context) error {
				canceled, cancel := context.WithCancelCause(ctx)
				cancel(tc.cause)
				_, got = unconfirmedDispatchFailure(canceled, context.Canceled, dispatcher.Report{})
				return nil
			}, activity.RegisterOptions{Name: "unconfirmed-dispatch-failure"})
			if _, err := env.ExecuteActivity("unconfirmed-dispatch-failure"); err != nil {
				t.Fatal(err)
			}
			if tc.wantClass == "" {
				if got != context.Canceled { //nolint:errorlint // the SDK keys requested cancellation on this exact value
					t.Fatalf("requested cancellation = %v, want context.Canceled", got)
				}
				return
			}
			converter := temporal.GetDefaultFailureConverter()
			wire := converter.FailureToError(converter.ErrorToFailure(got))
			class, err := ClassifyDispatchFailure(wire)
			if err != nil || class != tc.wantClass || !isWorkerLoss(wire) || errors.Is(wire, context.Canceled) {
				t.Fatalf("lost control path = %v (class %q, %v, worker loss %t)", wire, class, err, isWorkerLoss(wire))
			}
		})
	}
}

// Worker loss is retried on the infrastructure budget for a stage without
// policy actions, is still refused for a side-effecting stage, and a
// cancellation still ends the attempt loop without a retry.
func TestWorkerLossRetriesAsInfrastructureUnlessSideEffecting(t *testing.T) {
	converter := temporal.GetDefaultFailureConverter()
	lost := converter.FailureToError(converter.ErrorToFailure(dispatchControlPathLost(errors.New("worker is now shutdown"), context.Canceled)))
	for _, tc := range []struct {
		name          string
		err           error
		policyActions []string
		legacy        bool
		wantClasses   []journal.AttemptClass
		wantErr       string
	}{
		{name: "heartbeat timeout", err: temporal.NewTimeoutError(enumspb.TIMEOUT_TYPE_HEARTBEAT, nil), wantClasses: []journal.AttemptClass{"", journal.AttemptInfra}},
		{name: "lost control path", err: lost, wantClasses: []journal.AttemptClass{"", journal.AttemptInfra}},
		{name: "legacy heartbeat timeout", err: temporal.NewTimeoutError(enumspb.TIMEOUT_TYPE_HEARTBEAT, nil), legacy: true, wantClasses: []journal.AttemptClass{""}, wantErr: "attempt 1/1"},
		{name: "start-to-close timeout", err: temporal.NewTimeoutError(enumspb.TIMEOUT_TYPE_START_TO_CLOSE, nil), wantClasses: []journal.AttemptClass{""}, wantErr: "attempt 1/1"},
		{name: "side-effecting lost control path", err: lost, policyActions: []string{"external-mutation"}, wantClasses: []journal.AttemptClass{""}, wantErr: "refusing to retry after worker loss"},
		{name: "cancellation", err: temporal.NewCanceledError(), wantClasses: []journal.AttemptClass{""}, wantErr: "canceled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var classes []journal.AttemptClass
			var suite testsuite.WorkflowTestSuite
			env := temporaltest.NewWorkflowEnvironment(&suite)
			if tc.legacy {
				env.OnGetVersion(heartbeatLossInfraChange, workflow.DefaultVersion, 1).Return(workflow.DefaultVersion)
			}
			env.ExecuteWorkflow(func(ctx workflow.Context) error {
				task := retrySpec(nil).Tasks[0]
				task.PolicyActions = tc.policyActions
				_, err := dispatchWithRetry(ctx, RunInput{}, task, &runJournal{}, nil, func(_ workflow.Context, _ int, class journal.AttemptClass) (stageActivityResult, error) {
					classes = append(classes, class)
					if len(classes) == 1 {
						return stageActivityResult{}, tc.err
					}
					return stageActivityResult{ResultEnvelope: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}}, nil
				}, nil)
				return err
			})
			err := env.GetWorkflowError()
			if tc.wantErr == "" && err != nil || tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("workflow error = %v, want %q", err, tc.wantErr)
			}
			if len(classes) != len(tc.wantClasses) {
				t.Fatalf("dispatched classes = %q, want %q", classes, tc.wantClasses)
			}
			for i := range classes {
				if classes[i] != tc.wantClasses[i] {
					t.Fatalf("dispatched classes = %q, want %q", classes, tc.wantClasses)
				}
			}
		})
	}
}

// End to end through the run workflow: a pod stage whose dispatch heartbeat
// is lost is re-dispatched as infrastructure and the run completes.
func TestRunRedispatchesPodStageAfterHeartbeatLoss(t *testing.T) {
	in := runInput("heartbeat-loss", apiv1.WorkflowSpec{
		Gaggle: "web", Start: "build", Tasks: []apiv1.Task{podTask("build", "", nil)},
	})
	in.Placements = []PinnedPlacement{remotePin("build")}
	var suite testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&suite)
	env.RegisterActivity(&Activities{Workspaces: testWorkspaces(t)})
	var classes []journal.AttemptClass
	timeout := temporal.NewTimeoutError(enumspb.TIMEOUT_TYPE_HEARTBEAT, nil)
	env.OnActivity(ActDispatchStage, mock.Anything, mock.Anything).Return(func(_ context.Context, input DispatchStageInput) (stageActivityResult, error) {
		classes = append(classes, input.Class)
		if len(classes) == 1 {
			return stageActivityResult{}, timeout
		}
		return stageActivityResult{ResultEnvelope: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}}, nil
	})
	env.ExecuteWorkflow(Run, in)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("run after heartbeat loss: %v", err)
	}
	if len(classes) != 2 || classes[1] != journal.AttemptInfra {
		t.Fatalf("dispatched classes = %q, want an infrastructure retry", classes)
	}
}

// A retry refused because the prior attempt's writable pod still runs waits
// for that pod's deadline without spending the infrastructure budget the lost
// attempt already charged; repeated refusals stay bounded.
func TestHeldPriorAttemptDefersRetryToItsDeadline(t *testing.T) {
	converter := temporal.GetDefaultFailureConverter()
	for _, tc := range []struct {
		name        string
		deferrals   int
		wantClasses []journal.AttemptClass
		wantErr     string
	}{
		{name: "deferred once then succeeds", deferrals: 1, wantClasses: []journal.AttemptClass{"", journal.AttemptInfra, journal.AttemptInfra}},
		{name: "deferrals are bounded", deferrals: 5, wantClasses: []journal.AttemptClass{"", journal.AttemptInfra, journal.AttemptInfra, journal.AttemptInfra}, wantErr: "attempt 2/2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var classes []journal.AttemptClass
			var refusedAt, retriedAt time.Time
			var suite testsuite.WorkflowTestSuite
			env := temporaltest.NewWorkflowEnvironment(&suite)
			env.ExecuteWorkflow(func(ctx workflow.Context) error {
				_, err := dispatchWithRetry(ctx, RunInput{}, retrySpec(nil).Tasks[0], &runJournal{}, nil, func(ctx workflow.Context, _ int, class journal.AttemptClass) (stageActivityResult, error) {
					classes = append(classes, class)
					switch {
					case len(classes) == 1:
						return stageActivityResult{}, temporal.NewTimeoutError(enumspb.TIMEOUT_TYPE_HEARTBEAT, nil)
					case len(classes) <= 1+tc.deferrals:
						refusedAt = workflow.Now(ctx)
						held := &dispatcher.PriorAttemptLiveError{RunID: "run", Stage: "build", Pod: "build-a1", RetryAt: refusedAt.Add(time.Hour)}
						return stageActivityResult{}, converter.FailureToError(converter.ErrorToFailure(classifyDispatchError(fmt.Errorf("dispatch: %w", held))))
					}
					retriedAt = workflow.Now(ctx)
					return stageActivityResult{ResultEnvelope: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}}, nil
				}, nil)
				return err
			})
			err := env.GetWorkflowError()
			if tc.wantErr == "" && err != nil || tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("workflow error = %v, want %q", err, tc.wantErr)
			}
			if strings.Join(classStrings(classes), ",") != strings.Join(classStrings(tc.wantClasses), ",") {
				t.Fatalf("dispatched classes = %q, want %q", classes, tc.wantClasses)
			}
			if tc.wantErr == "" && retriedAt.Sub(refusedAt) < time.Hour {
				t.Fatalf("retried %v after the refusal, want the held pod's deadline", retriedAt.Sub(refusedAt))
			}
		})
	}
}

func classStrings(classes []journal.AttemptClass) []string {
	out := make([]string, len(classes))
	for i, class := range classes {
		out[i] = string(class)
	}
	return out
}
