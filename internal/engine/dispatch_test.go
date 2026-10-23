package engine

import (
	"context"
	"strings"
	"testing"

	"go.temporal.io/sdk/testsuite"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/temporaltest"
)

// TestDeterministicDispatchFailsClosed is #626's dispatch acceptance: an
// absent or zero-value DeterministicRun is a dispatch error — no empty
// command is ever executed. The registry rejects these shapes, so the
// fixtures build RunInput directly, the path a compromised or hand-built
// input would take.
func TestDeterministicDispatchFailsClosed(t *testing.T) {
	cases := []struct {
		name    string
		run     *apiv1.DeterministicRun
		wantErr string
	}{
		{"missing run", nil, "declares no DeterministicRun"},
		{"zero-value run", &apiv1.DeterministicRun{}, "refusing to dispatch an empty command"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := apiv1.WorkflowSpec{
				Gaggle:   "web",
				Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
				Start:    "lint",
				Tasks: []apiv1.Task{{
					Name: "lint", Type: apiv1.TaskDeterministic, Goal: "run lint", Run: tc.run,
				}},
			}
			det := &capturingDeterministic{}
			var ts testsuite.WorkflowTestSuite
			env := temporaltest.NewWorkflowEnvironment(&ts)
			env.RegisterActivity(&Activities{Det: det, Workspaces: testWorkspaces(t)})
			env.ExecuteWorkflow(Run, runInput("fail-closed-run", spec))
			err := env.GetWorkflowError()
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("workflow error = %v, want %q", err, tc.wantErr)
			}
			if got := len(det.captured()); got != 0 {
				t.Fatalf("executor dispatched %d times, want 0 — an empty command must never execute", got)
			}
		})
	}
}

func TestDeterministicScriptDispatch(t *testing.T) {
	const script = "echo inline"
	spec := apiv1.WorkflowSpec{
		Gaggle:   "web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerSchedule, Schedule: "@hourly"}},
		Start:    "check",
		Tasks: []apiv1.Task{{
			Name: "check",
			Type: apiv1.TaskDeterministic,
			Goal: "run inline check",
			Run:  &apiv1.DeterministicRun{Script: script, Workspace: apiv1.WorkspaceScratch},
		}},
	}

	det := &capturingDeterministic{}
	var ts testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&ts)
	env.RegisterActivity(&Activities{Det: det, Workspaces: testWorkspaces(t)})
	input := runInput("inline", spec)
	input.DSLVersion = "2.0"
	env.ExecuteWorkflow(Run, input)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	runs := det.capturedRuns()
	if len(runs) != 1 {
		t.Fatalf("deterministic dispatches = %d, want 1", len(runs))
	}
	if runs[0].Script != script || runs[0].Command != nil {
		t.Fatalf("dispatched run = %+v, want preserved inline script", runs[0])
	}
}

// TestRunDeterministicActivityRefusesEmptyCommand guards the dispatch
// boundary itself: even an activity invoked with a zero-value run (bypassing
// the workflow's own guard) fails closed before provisioning or executing.
func TestRunDeterministicActivityRefusesEmptyCommand(t *testing.T) {
	det := &capturingDeterministic{}
	workspaces := testWorkspaces(t)
	a := &Activities{Det: det, Workspaces: workspaces}
	_, err := a.RunDeterministic(context.Background(), apiv1.InvocationEnvelope{
		TaskID: "run-x:lint", RunID: "run-x", Gaggle: "web",
	}, apiv1.DeterministicRun{}, "", "")
	if err == nil || !strings.Contains(err.Error(), "refusing to execute (fail closed)") {
		t.Fatalf("activity error = %v, want the empty-command fail-closed error", err)
	}
	if got := len(det.captured()); got != 0 {
		t.Fatalf("executor dispatched %d times, want 0", got)
	}
	if got := len(workspaces.provisioned()); got != 0 {
		t.Fatalf("workspaces provisioned = %d, want 0 (no provisioning for a doomed dispatch)", got)
	}
}
