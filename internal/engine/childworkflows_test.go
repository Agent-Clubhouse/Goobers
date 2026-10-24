package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/engineadmission"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
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

func childCapableSpec() apiv1.WorkflowSpec {
	return apiv1.WorkflowSpec{Gaggle: "web", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}}, Start: "parent",
		Tasks: []apiv1.Task{{Name: "parent", Type: apiv1.TaskAgentic, Goal: "delegate", Goober: "coder", ChildWorkflows: &apiv1.ChildWorkflowPolicy{AllowedGoobers: []string{"coder"}}}}}
}

// The switch's version marker is recorded only for a run that pinned the
// opt-in: an opted-out child-capable run, refused as before, leaves its
// history byte-identical to one recorded before the switch existed.
func TestChildWorkflowsSwitchMarksOnlyOptedInRuns(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		var ts testsuite.WorkflowTestSuite
		env := temporaltest.NewWorkflowEnvironment(&ts)
		markers := 0
		env.OnGetVersion(childWorkflowsSwitchChange, workflow.DefaultVersion, 1).Run(func(mock.Arguments) { markers++ }).Return(workflow.Version(1)).Maybe()
		env.ExecuteWorkflow(Run, RunInput{RunID: "child-switch-marker", WorkflowName: "children", DSLVersion: "3.1", Spec: childCapableSpec(), ChildWorkflowsEnabled: enabled})
		if want := map[bool]int{false: 0, true: 1}[enabled]; markers != want {
			t.Fatalf("enabled=%v: switch markers = %d, want %d", enabled, markers, want)
		}
	}
}

// An opted-in run runs the child protocol itself, not merely past the start
// refusal: its first child-capable stage start asks the journal writer for a
// child origin, which only the active protocol does.
func TestChildWorkflowsSwitchActivatesTheProtocolAtTheFirstChildCapableStage(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		var ts testsuite.WorkflowTestSuite
		env := temporaltest.NewWorkflowEnvironment(&ts)
		env.RegisterActivity(&Activities{})
		var bound, started, seq int
		env.OnActivity(ActEmitJournal, mock.Anything, mock.Anything).Return(func(_ context.Context, req livejournal.EmitRequest) (livejournal.EmitResponse, error) {
			stage := false
			for _, op := range req.Ops {
				if op.Event != nil && op.Event.Type == journal.EventStageStarted && op.Event.Stage == "parent" {
					started, stage = started+1, true
					if op.ChildStageStart != nil {
						bound++
					}
				}
			}
			if stage {
				// Stop the run here: the stage start is what this test observes.
				return livejournal.EmitResponse{}, temporal.NewNonRetryableApplicationError("stop after the stage start", "test", nil)
			}
			seq += len(req.Ops)
			return livejournal.EmitResponse{Seq: uint64(seq)}, nil
		})
		spec := childCapableSpec()
		env.ExecuteWorkflow(Run, RunInput{RunID: "child-switch-active", WorkflowName: "children", DSLVersion: "3.1", Spec: spec, LiveJournal: true, ChildWorkflowsEnabled: enabled})
		if !enabled {
			if started != 0 {
				t.Fatalf("an opted-out child-capable run started its stage (%d)", started)
			}
			continue
		}
		if started == 0 || bound != started {
			t.Fatalf("opted in: %d of %d child-capable stage starts requested a child origin; want all (the protocol is not active)", bound, started)
		}
	}
}
