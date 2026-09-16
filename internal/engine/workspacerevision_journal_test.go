package engine

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/testsuite"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/temporaltest"
	wf "github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/workspacerevision"
)

func TestWorkspaceRevisionPodAwaitsDurableJournal(t *testing.T) {
	for _, live := range []bool{false, true} {
		name := "projection-only-refused"
		if live {
			name = "live-acknowledged"
		}
		t.Run(name, func(t *testing.T) {
			spec := fixtureSpec("select", []apiv1.Task{
				{Name: "select", Type: apiv1.TaskDeterministic, Goal: "select",
					Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}, Next: "inspect"},
				{Name: "inspect", Type: apiv1.TaskDeterministic, Goal: "inspect",
					Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceRepoReadOnly}, Next: wf.TerminalComplete},
			}, nil)
			in := runInput("durable-revision", spec)
			in.LiveJournal = live
			in.Placements = []PinnedPlacement{{Stage: "inspect", Queue: "pod"}}
			var acknowledged atomic.Bool
			var dispatched atomic.Int32
			emitter := receiptCleanupEmitter(func(_ context.Context, req livejournal.EmitRequest) (livejournal.EmitResponse, error) {
				for _, op := range req.Ops {
					if op.Event != nil && op.Event.Type == journal.EventStageFinished && op.Event.WorkspaceRevision != nil {
						time.Sleep(20 * time.Millisecond)
						acknowledged.Store(true)
					}
				}
				return livejournal.EmitResponse{Applied: len(req.Ops)}, nil
			})
			runner := &fakeRunner{run: func(context.Context, apiv1.InvocationEnvelope, apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
				return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, WorkspaceRevision: selectedRevisionFixture()}, nil
			}}
			var suite testsuite.WorkflowTestSuite
			env := temporaltest.NewWorkflowEnvironment(&suite)
			env.RegisterActivity(&Activities{Det: runner, Workspaces: testWorkspaces(t), Journal: emitter})
			env.OnActivity(ActDispatchStage, mock.Anything, mock.Anything).Return(
				func(_ context.Context, input DispatchStageInput) (stageActivityResult, error) {
					dispatched.Add(1)
					if !acknowledged.Load() {
						t.Error("pod credential acquisition could precede durable selected-control acknowledgment")
					}
					if input.WorkspaceRevision == nil {
						t.Error("pod dispatch omitted accepted revision")
					}
					return stageActivityResult{ResultEnvelope: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}}, nil
				})
			env.ExecuteWorkflow(Run, in)
			err := env.GetWorkflowError()
			if live {
				if err != nil || dispatched.Load() != 1 {
					t.Fatalf("live dispatch = %d, error = %v", dispatched.Load(), err)
				}
			} else if err == nil || !strings.Contains(err.Error(), workspacerevision.CodeInvalid) || dispatched.Load() != 0 {
				t.Fatalf("projection-only dispatch = %d, error = %v", dispatched.Load(), err)
			}
		})
	}
}
