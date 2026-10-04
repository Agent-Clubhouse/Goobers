package engine

import (
	"errors"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/temporaltest"
	wf "github.com/goobers/goobers/internal/workflow"
	"go.temporal.io/sdk/testsuite"
)

func TestChildWorkflowExecutionRefusedAtEngineBoundaries(t *testing.T) {
	def := wf.Definition{Name: "children", DSLVersion: "3.1", Spec: apiv1.WorkflowSpec{
		Gaggle: "web", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}}, Start: "parent",
		Tasks: []apiv1.Task{{Name: "parent", Type: apiv1.TaskAgentic, Goal: "delegate", Goober: "coder", ChildWorkflows: &apiv1.ChildWorkflowPolicy{AllowedGoobers: []string{"coder"}}}},
	}}
	reg := NewRegistryWithPreviewFeatures(true)
	if _, err := reg.RegisterDefinition(def); err != nil {
		t.Fatalf("policy registration should remain possible: %v", err)
	}
	if _, err := reg.StartInput(def.Name, StartSpec{RunID: "child-guard", Gaggle: "web"}); !errors.Is(err, wf.ErrChildWorkflowExecutionUnsupported) {
		t.Fatalf("StartInput: %v", err)
	}
	if err := RefuseDefinition(def.Name, def.Spec); !errors.Is(err, wf.ErrChildWorkflowExecutionUnsupported) {
		t.Fatalf("daemon preflight: %v", err)
	}
	// Direct Temporal invocation cannot bypass registry admission. No activities
	// are registered: refusal must happen before journaling or stage execution.
	var ts testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&ts)
	env.ExecuteWorkflow(Run, RunInput{RunID: "child-guard", WorkflowName: def.Name, DSLVersion: def.DSLVersion, Spec: def.Spec})
	if err := env.GetWorkflowError(); err == nil || !strings.Contains(err.Error(), wf.ErrChildWorkflowExecutionUnsupported.Error()) {
		t.Fatalf("direct Temporal run: %v", err)
	}
}
