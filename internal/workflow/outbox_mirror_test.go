package workflow

import (
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestCompileResolvesWorkflowOutboxMirrorWithTaskPrecedence(t *testing.T) {
	def := Definition{
		Name:    "outbox-mirror",
		Version: 1,
		Spec: apiv1.WorkflowSpec{
			Gaggle:           "example",
			Triggers:         []apiv1.Trigger{{Type: apiv1.TriggerManual}},
			Start:            "first",
			OutboxMirrorPath: "/workflow",
			Tasks: []apiv1.Task{
				{Name: "first", Type: apiv1.TaskDeterministic, Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: "second"},
				{Name: "second", Type: apiv1.TaskDeterministic, Run: &apiv1.DeterministicRun{Command: []string{"true"}}, OutboxMirrorPath: "/task"},
			},
		},
	}

	machine, err := Compile(def, WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	first, _ := machine.Task("first")
	second, _ := machine.Task("second")
	if first.OutboxMirrorPath != "/workflow" {
		t.Fatalf("first mirror = %q, want /workflow", first.OutboxMirrorPath)
	}
	if second.OutboxMirrorPath != "/task" {
		t.Fatalf("second mirror = %q, want /task", second.OutboxMirrorPath)
	}
	if def.Spec.Tasks[0].OutboxMirrorPath != "" {
		t.Fatal("Compile mutated the caller's definition")
	}
}

// TestCompileRejectsInvalidOutboxDeclarations pins the #3662 boundary: an
// outbox entry that escapes the workspace (or is empty) and a mirror root that
// is not absolute or home-relative are compile errors, not runtime export
// failures after the stage has already done its work.
func TestCompileRejectsInvalidOutboxDeclarations(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*apiv1.WorkflowSpec)
		wantMsg string
	}{
		{
			name:    "escaping outbox entry",
			mutate:  func(s *apiv1.WorkflowSpec) { s.Tasks[0].Outbox = []string{"../outside"} },
			wantMsg: `task "first" outbox[0]`,
		},
		{
			name:    "empty outbox entry",
			mutate:  func(s *apiv1.WorkflowSpec) { s.Tasks[0].Outbox = []string{"report.md", ""} },
			wantMsg: `task "first" outbox[1]`,
		},
		{
			name:    "absolute outbox entry",
			mutate:  func(s *apiv1.WorkflowSpec) { s.Tasks[0].Outbox = []string{"/etc/passwd"} },
			wantMsg: `task "first" outbox[0]`,
		},
		{
			name:    "relative workflow mirror root",
			mutate:  func(s *apiv1.WorkflowSpec) { s.OutboxMirrorPath = "reports" },
			wantMsg: "spec.outboxMirrorPath",
		},
		{
			name:    "relative task mirror root",
			mutate:  func(s *apiv1.WorkflowSpec) { s.Tasks[0].OutboxMirrorPath = "./reports" },
			wantMsg: `task "first" outboxMirrorPath`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			def := Definition{
				Name:    "outbox",
				Version: 1,
				Spec: apiv1.WorkflowSpec{
					Gaggle:   "example",
					Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
					Start:    "first",
					Tasks: []apiv1.Task{
						{Name: "first", Type: apiv1.TaskDeterministic, Run: &apiv1.DeterministicRun{Command: []string{"true"}}},
					},
				},
			}
			tc.mutate(&def.Spec)

			_, err := Compile(def, WithPreviewFeatures(true))
			if err == nil {
				t.Fatal("Compile succeeded; want an outbox validation error")
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("Compile error %v; want it to name %q", err, tc.wantMsg)
			}
		})
	}
}

// TestCompileAcceptsValidOutboxDeclarations proves the guard leaves working
// declarations alone: contained entries and a home-relative mirror root
// compile as before.
func TestCompileAcceptsValidOutboxDeclarations(t *testing.T) {
	def := Definition{
		Name:    "outbox",
		Version: 1,
		Spec: apiv1.WorkflowSpec{
			Gaggle:           "example",
			Triggers:         []apiv1.Trigger{{Type: apiv1.TriggerManual}},
			Start:            "first",
			OutboxMirrorPath: "~/goobers/outbox",
			Tasks: []apiv1.Task{
				{
					Name:   "first",
					Type:   apiv1.TaskDeterministic,
					Run:    &apiv1.DeterministicRun{Command: []string{"true"}},
					Outbox: []string{"report.md", "reports/summary.json"},
				},
			},
		},
	}

	if _, err := Compile(def, WithPreviewFeatures(true)); err != nil {
		t.Fatalf("Compile: %v", err)
	}
}
