package engine

import (
	"strings"
	"testing"

	"go.temporal.io/sdk/testsuite"

	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/temporaltest"
)

// Register production Run for BOTH the parent and its generated child. The
// legacy runWithChildWorkflows test entry supplied a context that cannot cross
// a real Temporal child-workflow boundary and masked this production path.
func TestProductionRunUsesFrozenChildPosture(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		name := "enabled"
		if !enabled {
			name = "disabled"
		}
		t.Run(name, func(t *testing.T) {
			f := newChildFixture()
			f.changeLoad = func(manifest *FrozenChildManifest) { manifest.Input.ChildWorkflowsEnabled = enabled }
			var suite testsuite.WorkflowTestSuite
			env := temporaltest.NewWorkflowEnvironment(&suite)
			f.now = env.Now
			writer, _ := newLiveWriter(t)
			env.RegisterWorkflow(Run)
			registerChildActivities(env, &Activities{EngineChildren: f, Journal: writer, Auto: gate.NewAutomatedEvaluator()}, f)
			in := childParentInput()
			in.ChildWorkflowsEnabled = true
			env.ExecuteWorkflow(Run, in)
			err := env.GetWorkflowError()
			if !enabled {
				if err == nil || !strings.Contains(err.Error(), "parent's pinned posture") {
					t.Fatalf("disabled frozen child = %v, want pin refusal", err)
				}
				if len(f.terminal) != 0 {
					t.Fatal("disabled frozen child executed")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(f.terminal) != 1 || len(f.envelopes) != 2 || len(f.admissions) != 1 || !f.admissions[0].ChildWorkflowsEnabled {
				t.Fatalf("production family did not execute from parent pin: terminal=%d dispatches=%d admissions=%+v", len(f.terminal), len(f.envelopes), f.admissions)
			}
		})
	}
}

func TestProductionRunDisabledParentCannotAdmitChild(t *testing.T) {
	f := newChildFixture()
	var suite testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&suite)
	env.RegisterWorkflow(Run)
	writer, _ := newLiveWriter(t)
	registerChildActivities(env, &Activities{EngineChildren: f, Journal: writer}, f)
	in := childParentInput()
	in.ChildWorkflowsEnabled = false
	env.ExecuteWorkflow(Run, in)
	if env.GetWorkflowError() == nil || len(f.admissions) != 0 || len(f.envelopes) != 0 {
		t.Fatal("disabled parent admitted or dispatched a child")
	}
}
