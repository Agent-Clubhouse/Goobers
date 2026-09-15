package coordination

import (
	"fmt"
	"reflect"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

const WorkflowKind = "coordination"

// ValidateWorkflow restricts native coordination to a single deterministic,
// manual task. File selectors are static and covered by the workflow digest.
func ValidateWorkflow(spec apiv1.WorkflowSpec) error {
	found := false
	for _, task := range spec.Tasks {
		found = found || task.Inputs["kind"] == WorkflowKind
	}
	if !found {
		return nil
	}
	if len(spec.Triggers) != 1 || spec.Triggers[0].Type != apiv1.TriggerManual || len(spec.Tasks) != 1 || len(spec.Gates) != 0 || len(spec.Parallels) != 0 {
		return fmt.Errorf("coordination requires a single-task manual-only workflow without gates or parallel branches")
	}
	task := spec.Tasks[0]
	if task.Type != apiv1.TaskDeterministic || task.Goober != "" || len(task.Capabilities) != 0 || len(task.InputsFrom) != 0 || task.Experiment != nil {
		return fmt.Errorf("coordination requires deterministic execution without stage capabilities, agentic tasks or dynamic inputs")
	}
	want := &apiv1.DeterministicRun{Command: []string{"goobers", "coordinate"}, Workspace: apiv1.WorkspaceScratch}
	if !reflect.DeepEqual(task.Run, want) || spec.Start != task.Name {
		return fmt.Errorf("coordination requires run.command [goobers, coordinate], workspace scratch, and no other run options")
	}
	if task.Inputs["planFile"] == "" {
		return fmt.Errorf("coordination requires a static planFile")
	}
	for key := range task.Inputs {
		switch key {
		case "kind", "planFile", "evidenceFile", "artifactFile":
		default:
			return fmt.Errorf("coordination rejects input %q", key)
		}
	}
	return nil
}
