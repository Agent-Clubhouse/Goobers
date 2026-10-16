package workflow

import (
	"errors"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestChildParallelWorkspaceAdmission(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Definition)
		want   string
	}{
		{"isolated sequential", func(*Definition) {}, ""},
		{"isolated concurrent", func(d *Definition) { d.Spec.Parallels[0].MaxConcurrentBranches = 2 }, ""},
		{"legacy pin", func(d *Definition) { d.DSLVersion = "3.0" }, "requires dslVersion"},
		{"without child policy", func(d *Definition) { d.Spec.Tasks[0].ChildWorkflows = nil }, "writable repo workspace"},
		{"join opt-in only", func(d *Definition) {
			d.Spec.Tasks[2].ChildWorkflows = d.Spec.Tasks[0].ChildWorkflows
			d.Spec.Tasks[0].ChildWorkflows = nil
		}, "writable repo workspace"},
		{"missing repo handoff", func(d *Definition) { d.Spec.Tasks[2].RepoFrom = nil }, "repoFrom"},
		{"human branch gate", func(d *Definition) {
			d.Spec.Tasks[0].Next = "approval"
			d.Spec.Gates = []apiv1.Gate{{Name: "approval", Evaluator: apiv1.EvaluatorHuman, Human: &apiv1.HumanGate{}, Branches: map[string]string{"pass": TargetJoin, "reject": TargetAbort}}}
		}, "human gates are not supported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := childParallelDefinition()
			tc.change(&d)
			_, err := Compile(d, WithPreviewFeatures(true))
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("compile=%v; want %s", err, tc.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !errors.Is(RefuseChildWorkflowExecution(d.Spec), ErrChildWorkflowExecutionUnsupported) {
				t.Fatal("compile acceptance bypassed public execution guard")
			}
			if _, err := Compile(d); err == nil || !strings.Contains(err.Error(), "preview") {
				t.Fatalf("missing preview opt-in accepted: %v", err)
			}
		})
	}
}

func childParallelDefinition() Definition {
	d := childPolicyDefinition()
	d.Spec.Start = "fan"
	producer := d.Spec.Tasks[0]
	producer.Name, producer.Workspace, producer.Next = "a", apiv1.WorkspaceRepo, TargetJoin
	d.Spec.Tasks = []apiv1.Task{
		producer,
		{Name: "b", Type: apiv1.TaskAgentic, Goal: "other branch", Goober: "coder", Workspace: apiv1.WorkspaceRepo, Next: TargetJoin},
		{Name: "join", Type: apiv1.TaskAgentic, Goal: "join", Goober: "coder", Workspace: apiv1.WorkspaceRepo, RepoFrom: apiv1.RepoFrom{"a", "b"}},
	}
	d.Spec.Parallels = []apiv1.Parallel{{Name: "fan", FailurePolicy: apiv1.BranchContinueOnError, MaxConcurrentBranches: 1, Join: "join", Branches: []apiv1.Branch{{Name: "left", Start: "a"}, {Name: "right", Start: "b"}}}}
	return d
}
