package engine

import (
	"context"
	"strings"
	"testing"

	"go.temporal.io/sdk/testsuite"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// TestRegisterRejectsSchemaInvalidShapes is #626's registry acceptance: the
// registry re-asserts the task shape invariants workflow.schema.json owns —
// definitions the schema would reject never enter the registry, with errors
// naming the violated invariant.
func TestRegisterRejectsSchemaInvalidShapes(t *testing.T) {
	valid := linearSpec()
	cases := []struct {
		name    string
		mutate  func(*apiv1.Task)
		wantErr string
	}{
		{
			name:    "agentic without goober",
			mutate:  func(task *apiv1.Task) { task.Goober = "" },
			wantErr: "agentic requires goober",
		},
		{
			name:    "agentic with run block",
			mutate:  func(task *apiv1.Task) { task.Run = &apiv1.DeterministicRun{Command: []string{"true"}} },
			wantErr: "agentic forbids run",
		},
		{
			name: "deterministic without run",
			mutate: func(task *apiv1.Task) {
				task.Type = apiv1.TaskDeterministic
				task.Goober = ""
				task.Run = nil
			},
			wantErr: "deterministic requires run",
		},
		{
			name: "deterministic with empty command",
			mutate: func(task *apiv1.Task) {
				task.Type = apiv1.TaskDeterministic
				task.Goober = ""
				task.Run = &apiv1.DeterministicRun{}
			},
			wantErr: "run.command requires at least one element",
		},
		{
			name: "deterministic with goober",
			mutate: func(task *apiv1.Task) {
				task.Type = apiv1.TaskDeterministic
				task.Run = &apiv1.DeterministicRun{Command: []string{"true"}}
			},
			wantErr: "deterministic forbids goober",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := valid
			spec.Tasks = append([]apiv1.Task(nil), valid.Tasks...)
			tc.mutate(&spec.Tasks[0])

			r := NewRegistryWithPreviewFeatures(true)
			_, err := r.Register("bad-shape", spec)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Register error = %v, want it to name the violated invariant %q", err, tc.wantErr)
			}
			if _, ok := r.Latest("bad-shape"); ok {
				t.Fatal("schema-invalid definition entered the registry")
			}
		})
	}
}

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
			env := ts.NewTestWorkflowEnvironment()
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

// TestRunDeterministicActivityRefusesEmptyCommand guards the dispatch
// boundary itself: even an activity invoked with a zero-value run (bypassing
// the workflow's own guard) fails closed before provisioning or executing.
func TestRunDeterministicActivityRefusesEmptyCommand(t *testing.T) {
	det := &capturingDeterministic{}
	workspaces := testWorkspaces(t)
	a := &Activities{Det: det, Workspaces: workspaces}
	_, err := a.RunDeterministic(context.Background(), apiv1.InvocationEnvelope{
		TaskID: "run-x:lint", RunID: "run-x", Gaggle: "web",
	}, apiv1.DeterministicRun{})
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

// TestRegistryStartInputCarriesPinnedPolicy: the StartSpec policy fields
// added for envelope parity thread through into the pinned RunInput.
func TestRegistryStartInputCarriesPinnedPolicy(t *testing.T) {
	r := NewRegistryWithPreviewFeatures(true)
	if _, err := r.Register("flow", linearSpec()); err != nil {
		t.Fatalf("Register: %v", err)
	}
	in, err := r.StartInput("flow", StartSpec{
		RunID:                  "run-1",
		Gaggle:                 "web",
		TriggerRef:             "item#9",
		BranchNamespace:        "goobers/",
		GateGooberCapabilities: map[string][]string{"reviewer": {"agent:model"}},
	})
	if err != nil {
		t.Fatalf("StartInput: %v", err)
	}
	if in.TriggerRef != "item#9" || in.BranchNamespace != "goobers/" {
		t.Fatalf("run input trigger/namespace = %q/%q, want item#9 / goobers/", in.TriggerRef, in.BranchNamespace)
	}
	if got := in.GateGooberCapabilities["reviewer"]; len(got) != 1 || got[0] != "agent:model" {
		t.Fatalf("run input gate goober capabilities = %v, want [agent:model]", got)
	}
}
