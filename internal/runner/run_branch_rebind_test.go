package runner

import (
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/workflow"
)

func TestReadOnlyRebindErrorNamesRebindingStage(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle:   "web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "select",
		Tasks: []apiv1.Task{{
			Name: "select", Type: apiv1.TaskDeterministic, Goal: "select",
			Run:             &apiv1.DeterministicRun{Command: []string{"./select.sh"}},
			ExpectedOutputs: []string{WorkspaceBranchOutput},
			Next:            "inspect",
		}, {
			Name: "inspect", Type: apiv1.TaskAgentic, Goal: "inspect", Goober: "reviewer",
			Workspace: apiv1.WorkspaceRepoReadOnly,
		}},
	}
	machine, err := workflow.Compile(workflow.Definition{Name: "review", Version: 1, Spec: spec}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	got := readOnlyRebindError(StartInput{Machine: machine}, "inspect", "feature/x").Error()
	for _, want := range []string{
		"create read-only workspace:",
		`stage "inspect" declares workspace: repo-readonly`,
		`rebound to "feature/x" by stage "select"`,
		`Move "inspect" before the rebinding stage, or set its workspace to repo`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("error missing %q: %s", want, got)
		}
	}

	// A continuation run's source branch rebinds with no rebinding stage in
	// the workflow; the error must still say where the branch came from.
	got = readOnlyRebindError(StartInput{}, "inspect", "feature/x").Error()
	if !strings.Contains(got, "run's source branch") {
		t.Errorf("error without a rebinding stage does not name the source: %s", got)
	}
}
