package shippedworkflows

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/fluidui"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/instance"
)

// TestFluidUICustomizeWorkflowContract pins #8418's safety shape: the agent
// stage only drafts JSON with model access, invalid output gets exactly one
// repass, and nothing in the workflow applies a candidate.
func TestFluidUICustomizeWorkflowContract(t *testing.T) {
	root := repositoryRoot(t)
	set, report, err := instance.LoadConfigDir(filepath.Join(root, "reference-workflows"))
	if err != nil {
		t.Fatalf("load reference workflows: %v\n%v", err, report)
	}
	spec := findWorkflow(t, set.Workflows, "goobers", fluidui.Workflow).Spec
	if len(spec.Triggers) != 1 || spec.Triggers[0].Type != apiv1.TriggerManual {
		t.Fatalf("triggers = %+v, want one manual trigger (the request intake starts it)", spec.Triggers)
	}
	if spec.Start != fluidui.LoadStage || spec.RunControls == nil || spec.RunControls.MaxRepasses != 1 {
		t.Fatalf("entry/run controls = %+v", spec)
	}

	load := requireTask(t, spec, fluidui.LoadStage)
	propose := requireTask(t, spec, "propose")
	validate := requireTask(t, spec, fluidui.ValidateStage)
	if load.Next != "propose" || propose.Next != fluidui.ValidateStage || validate.Next != "candidate-valid" || len(spec.Tasks) != 3 {
		t.Fatalf("unexpected stage wiring: %d tasks", len(spec.Tasks))
	}
	if !reflect.DeepEqual(load.Run.Command, []string{"goobers", "fluid-ui-request", "--load"}) ||
		!reflect.DeepEqual(validate.Run.Command, []string{"goobers", "fluid-ui-request", "--validate"}) {
		t.Fatalf("deterministic commands = %v / %v", load.Run.Command, validate.Run.Command)
	}
	if propose.Goober != "fluid-ui-designer" || propose.Workspace != apiv1.WorkspaceScratch ||
		!reflect.DeepEqual(propose.Capabilities, []string{"agent:model"}) || len(propose.PolicyActions) != 0 ||
		propose.Inputs["artifactFile"] != "fluid-ui-candidate.json" ||
		!reflect.DeepEqual(propose.ContextFrom, []string{fluidui.LoadStage, fluidui.ValidateStage}) {
		t.Fatalf("propose is not a confined JSON-drafting stage: %+v", propose)
	}
	for _, task := range spec.Tasks {
		if len(task.PolicyActions) != 0 || (task.Type == apiv1.TaskDeterministic && len(task.Capabilities) != 0) {
			t.Fatalf("stage %q holds grants beyond drafting: %+v", task.Name, task)
		}
	}

	candidateGate := requireGate(t, spec, "candidate-valid")
	if candidateGate.Automated.Params["key"] != "repassable" || candidateGate.Automated.Params["equals"] != "false" ||
		candidateGate.MaxRepasses != 1 || candidateGate.Branches["pass"] != "" ||
		candidateGate.Branches["fail"] != "propose" || candidateGate.Branches["escalate"] != "@abort" {
		t.Fatalf("candidate-valid gate = %+v", candidateGate)
	}
	assertOutputGateTarget(t, candidateGate, map[string]interface{}{"repassable": true}, gate.OutcomeFail, "propose")
	assertOutputGateTarget(t, candidateGate, map[string]interface{}{"repassable": false}, gate.OutcomePass, "")

	var designer apiv1.Goober
	for _, candidate := range set.Goobers {
		if candidate.Name == "fluid-ui-designer" {
			designer = candidate
		}
	}
	if !reflect.DeepEqual(designer.Spec.Capabilities, []string{"agent:model"}) || len(designer.Spec.Tools) != 0 ||
		!reflect.DeepEqual(designer.Spec.Workflows, []string{fluidui.Workflow}) {
		t.Fatalf("fluid-ui-designer grants = %+v", designer.Spec)
	}
	instructions, err := os.ReadFile(filepath.Join(root, "reference-workflows", "gaggles", "goobers", "goobers", "fluid-ui-designer", "instructions.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"fluid-ui-candidate.json", `"unsupported": true`, "You never write code", "Never copy the request"} {
		if !strings.Contains(string(instructions), want) {
			t.Fatalf("fluid-ui-designer instructions missing %q", want)
		}
	}
}
