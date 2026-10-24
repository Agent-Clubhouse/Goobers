package engine

import (
	"errors"
	"strings"
	"testing"

	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/engineadmission"
	"github.com/goobers/goobers/internal/temporaltest"
	wf "github.com/goobers/goobers/internal/workflow"
)

func TestChildWorkflowExecutionRefusedAtEngineBoundaries(t *testing.T) {
	def := wf.Definition{Name: "children", DSLVersion: "3.1", Spec: apiv1.WorkflowSpec{
		Gaggle: "web", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}}, Start: "parent",
		Tasks: []apiv1.Task{{Name: "parent", Type: apiv1.TaskAgentic, Goal: "delegate", Goober: "coder", ChildWorkflows: &apiv1.ChildWorkflowPolicy{AllowedGoobers: []string{"coder"}}}},
	}}
	if err := engineadmission.RefuseDefinition(def.Name, def.Spec); !errors.Is(err, wf.ErrChildWorkflowExecutionUnsupported) {
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

// The production switch (#8085 d): a run that pinned the instance's opt-in
// starts past the child-workflow refusal; a run without it, or a history
// recorded before the switch, is refused before any stage.
func TestChildWorkflowsSwitchLiftsOnlyThePinnedOptIn(t *testing.T) {
	spec := apiv1.WorkflowSpec{Gaggle: "web", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}}, Start: "parent",
		Tasks: []apiv1.Task{{Name: "parent", Type: apiv1.TaskAgentic, Goal: "delegate", Goober: "coder", ChildWorkflows: &apiv1.ChildWorkflowPolicy{AllowedGoobers: []string{"coder"}}}}}
	refused := func(err error) bool {
		return err != nil && strings.Contains(err.Error(), wf.ErrChildWorkflowExecutionUnsupported.Error())
	}
	run := func(t *testing.T, enabled bool, prior bool) error {
		t.Helper()
		var ts testsuite.WorkflowTestSuite
		env := temporaltest.NewWorkflowEnvironment(&ts)
		if prior {
			// Replaying a history recorded before the switch existed.
			env.OnGetVersion(childWorkflowsSwitchChange, workflow.DefaultVersion, 1).Return(workflow.DefaultVersion)
		}
		env.ExecuteWorkflow(Run, RunInput{RunID: "child-switch", WorkflowName: "children", DSLVersion: "3.1", Spec: spec, ChildWorkflowsEnabled: enabled})
		return env.GetWorkflowError()
	}
	if err := run(t, false, false); !refused(err) {
		t.Fatalf("switch off: %v, want the child-workflow refusal", err)
	}
	if err := run(t, true, true); !refused(err) {
		t.Fatalf("opted in, replaying a pre-switch history: %v, want the child-workflow refusal", err)
	}
	// No activities are registered, so the opted-in run fails at its first
	// stage, past the start refusal.
	err := run(t, true, false)
	if refused(err) {
		t.Fatalf("opted in: %v, want the run to start past the refusal", err)
	}
	t.Logf("opted-in run stopped past the start refusal at: %v", err)
}
