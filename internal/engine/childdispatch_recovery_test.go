package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/mock"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

type childRecoveryDispatcher struct {
	childWireDispatcher
	reconciled int
}

func (d *childRecoveryDispatcher) ReconcileChildPod(_ context.Context, a dispatcher.Attempt, custody dispatcher.ChildPodCustody) (dispatcher.Report, error) {
	d.reconciled++
	d.attempt = a
	return dispatcher.Report{ChildCreateAttempted: true, ChildPodUID: custody.UID, WorkspaceWritersStopped: true, SurrenderConfirmed: true, Disposed: true}, errors.New("lost worker")
}

func TestChildDispatchLossOnlyReconcilesBoundHeartbeatCustody(t *testing.T) {
	for _, scenario := range []string{"bound", "missing", "changed", "no-uid", "ordinary-failure", "legacy"} {
		t.Run(scenario, func(t *testing.T) {
			in := ChildDispatchInput{Attempt: dispatcher.Attempt{RunID: "run", Gaggle: "g", Stage: "work", Number: 1, PodAttempt: 1, ChildExecutionDigest: journal.Digest([]byte("contract"))}, Eligible: []dispatcher.RunnerSpec{{Name: "linux", OS: "linux", HostKind: instance.RunnerHostImage, Host: "image:v1"}}, Queue: "q"}
			custody := ChildDispatchCustody{BindingDigest: in.BindingDigest(), Pod: dispatcher.ChildPodCustody{Namespace: "n", Name: dispatcher.PodName(in.Attempt), UID: "original"}}
			var failure error
			switch scenario {
			case "missing":
				failure = temporal.NewTimeoutError(enumspb.TIMEOUT_TYPE_HEARTBEAT, nil)
			case "ordinary-failure":
				failure = errors.New("ordinary failure")
			default:
				if scenario == "changed" {
					custody.BindingDigest = journal.Digest([]byte("other"))
				}
				if scenario == "no-uid" {
					custody.Pod.UID = ""
				}
				failure = temporal.NewTimeoutError(enumspb.TIMEOUT_TYPE_HEARTBEAT, nil, custody)
			}
			var suite testsuite.WorkflowTestSuite
			env := suite.NewTestWorkflowEnvironment()
			env.SetStartWorkflowOptions(client.StartWorkflowOptions{ID: ChildDispatchWorkflowID(in.Attempt)})
			d := &childRecoveryDispatcher{}
			activities := &Activities{Dispatcher: d}
			env.RegisterActivity(activities)
			if scenario == "legacy" {
				env.OnGetVersion("isolated-child-custody-recovery-v1", workflow.DefaultVersion, 1).Return(workflow.DefaultVersion)
			}
			env.OnActivity(activities.DispatchChildPod, mock.Anything, mock.Anything).Return(ChildDispatchResult{}, failure)
			env.ExecuteWorkflow(ChildDispatchOne, in)
			if scenario != "bound" {
				if env.GetWorkflowError() == nil || d.reconciled != 0 || d.calls != 0 {
					t.Fatal("unbound custody recovered", env.GetWorkflowError(), d)
				}
				return
			}
			var result ChildDispatchResult
			if err := env.GetWorkflowResult(&result); err != nil {
				t.Fatal(err)
			}
			if d.reconciled != 1 || d.calls != 0 || result.BindingDigest != in.BindingDigest() || result.Report.ChildPodUID != "original" || !result.Report.WorkspaceWritersStopped || d.attempt.OwningWorkflowID != ChildDispatchWorkflowID(in.Attempt) {
				t.Fatal("recovery repeated dispatch or lost identity", d, result)
			}
		})
	}
}
