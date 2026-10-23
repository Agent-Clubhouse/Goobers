package engine

import (
	"errors"
	"testing"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/temporaltest"
	"github.com/goobers/goobers/internal/usageallowance"
)

func TestCancelSafeStageJournalVersionReplaysLegacyHistory(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&suite)
	env.OnGetVersion(
		cancelSafeStageJournalChange,
		workflow.DefaultVersion, 1,
	).Return(workflow.DefaultVersion)
	env.RegisterActivity(&Activities{
		Det:        &scriptedStages{},
		Workspaces: testWorkspaces(t),
	})
	in := runInput("cancel-safe-journal-legacy", twoStageCancelSpec())
	env.ExecuteWorkflow(Run, in)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	value, err := env.QueryWorkflow(JournalQuery)
	if err != nil {
		t.Fatal(err)
	}
	var projection JournalProjection
	if err := value.Get(&projection); err != nil {
		t.Fatal(err)
	}
	var finished int
	for _, op := range projection.Ops {
		if op.Event != nil && op.Event.Type == journal.EventStageFinished {
			finished++
		}
	}
	if finished != 2 {
		t.Fatalf("legacy replay stage.finished count = %d, want 2", finished)
	}
}

func TestSameWorkflowTaskCancelAfterCommitJournalsSuccess(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&suite)
	var got apiv1.ResultEnvelope
	var gotErr error
	var finished int
	env.ExecuteWorkflow(func(ctx workflow.Context) error {
		ctx = workflow.WithValue(ctx, cancelSafeStageJournalKey{}, true)
		ctx, cancel := workflow.WithCancel(ctx)
		rec := &runJournal{}
		got, gotErr = dispatchWithRetry(
			ctx, RunInput{}, retrySpec(nil).Tasks[0], rec, nil,
			func(
				workflow.Context,
				int,
				journal.AttemptClass,
				*usageallowance.Allowance,
			) (stageActivityResult, error) {
				cancel()
				return stageActivityResult{ResultEnvelope: apiv1.ResultEnvelope{
					Status: apiv1.ResultSuccess,
				}}, nil
			},
			nil,
		)
		for _, op := range rec.proj.Ops {
			if op.Event != nil &&
				op.Event.Type == journal.EventStageFinished {
				finished++
			}
		}
		return nil
	})
	if finished != 1 || !temporal.IsCanceledError(gotErr) ||
		got.Status != "" {
		t.Fatalf("stage.finished=%d err=%v status=%q",
			finished, gotErr, got.Status)
	}
}

func TestAcknowledgedExecutorCancelDoesNotJournalSuccess(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&suite)
	var gotErr error
	var finished int
	env.ExecuteWorkflow(func(ctx workflow.Context) error {
		ctx = workflow.WithValue(ctx, cancelSafeStageJournalKey{}, true)
		ctx, cancel := workflow.WithCancel(ctx)
		rec := &runJournal{}
		_, gotErr = dispatchWithRetry(
			ctx, RunInput{}, retrySpec(nil).Tasks[0], rec, nil,
			func(
				workflow.Context,
				int,
				journal.AttemptClass,
				*usageallowance.Allowance,
			) (stageActivityResult, error) {
				cancel()
				return stageActivityResult{}, temporal.NewCanceledError()
			},
			nil,
		)
		for _, op := range rec.proj.Ops {
			if op.Event != nil &&
				op.Event.Type == journal.EventStageFinished {
				finished++
			}
		}
		return nil
	})
	if finished != 0 || !temporal.IsCanceledError(gotErr) {
		t.Fatalf("stage.finished=%d err=%v", finished, gotErr)
	}
}

func TestCancelSafeStageWaitsForActivityCancellationOutcome(t *testing.T) {
	for _, test := range []struct {
		name string
		gate bool
		want bool
	}{
		{name: "legacy", gate: false, want: false},
		{name: "cancel-safe", gate: true, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var suite testsuite.WorkflowTestSuite
			env := temporaltest.NewWorkflowEnvironment(&suite)
			env.ExecuteWorkflow(func(ctx workflow.Context) error {
				ctx = workflow.WithValue(
					ctx, cancelSafeStageJournalKey{}, test.gate)
				options := workflow.GetActivityOptions(
					stageActivityContext(ctx, apiv1.Limits{}))
				if options.WaitForCancellation != test.want {
					return errors.New("unexpected WaitForCancellation")
				}
				return nil
			})
			if err := env.GetWorkflowError(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func twoStageCancelSpec() apiv1.WorkflowSpec {
	return apiv1.WorkflowSpec{
		Gaggle: "web",
		Start:  "first",
		Tasks: []apiv1.Task{
			{
				Name: "first", Type: apiv1.TaskDeterministic,
				Goal: "first", Next: "second",
				Run: &apiv1.DeterministicRun{
					Command:   []string{"true"},
					Workspace: apiv1.WorkspaceScratch,
				},
			},
			{
				Name: "second", Type: apiv1.TaskDeterministic,
				Goal: "second",
				Run: &apiv1.DeterministicRun{
					Command:   []string{"true"},
					Workspace: apiv1.WorkspaceScratch,
				},
			},
		},
	}
}
