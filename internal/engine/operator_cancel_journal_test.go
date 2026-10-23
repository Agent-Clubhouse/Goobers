package engine

import (
	"testing"

	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/temporaltest"
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
