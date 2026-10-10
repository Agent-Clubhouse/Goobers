package runner

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/workflow"
)

func TestParallelSchedulingKeepsChildWaitsOffSerialPath(t *testing.T) {
	for _, tc := range []struct {
		name  string
		width int32
		child string
		want  bool
	}{
		{"ordinary-single-slot", 1, "", false},
		{"ordinary-concurrent", 2, "", true},
		{"child-after-ordinary-stage", 1, "lens-a", true},
		{"child-in-join-only", 1, "collate", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			def := parallelRunnerMachine(t, tc.width, apiv1.WorkspaceScratch).Def
			def.DSLVersion = "3.1"
			for i := range def.Spec.Tasks {
				task := &def.Spec.Tasks[i]
				if task.Name == tc.child {
					task.Type, task.Run, task.Goober, task.Workspace = apiv1.TaskAgentic, nil, "coder", apiv1.WorkspaceScratch
					task.ChildWorkflows = &apiv1.ChildWorkflowPolicy{AllowedGoobers: []string{"coder"}}
				}
			}
			// The opt-in is deliberately not a branch entry; scheduling must inspect
			// transitions inside the branch rather than just its first task.
			def.Spec.Tasks = append(def.Spec.Tasks, apiv1.Task{Name: "prepare-a", Type: apiv1.TaskDeterministic, Goal: "prepare", Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}, Next: "lens-a"})
			def.Spec.Parallels[0].Branches[0].Start = "prepare-a"
			machine, err := workflow.Compile(def, workflow.WithPreviewFeatures(true))
			if err != nil {
				t.Fatal(err)
			}
			parallel, _ := machine.Parallel("fan")
			if got := parallelUsesDispatcher(machine, parallel); got != tc.want {
				t.Fatalf("dispatcher=%v; want %v", got, tc.want)
			}
			// Choosing the dispatcher is not permission to execute child workflows.
			if tc.child != "" && workflow.RefuseChildWorkflowExecution(machine.Def.Spec) == nil {
				t.Fatal("scheduling change bypassed public child admission")
			}
		})
	}
}
