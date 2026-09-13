package workflow

import (
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestImplicitWritableWorkspaceWarnings(t *testing.T) {
	tests := []struct {
		name     string
		def      Definition
		goobers  map[string]apiv1.GooberSpec
		want     bool
		wantText []string
	}{
		{
			name: "read-only deterministic command",
			def: Definition{Name: "inspect", Spec: apiv1.WorkflowSpec{Tasks: []apiv1.Task{{
				Name: "tests", Type: apiv1.TaskDeterministic,
				Run: &apiv1.DeterministicRun{Command: []string{"go", "test", "./..."}},
			}}}},
			want: true,
			wantText: []string{
				"deterministic task",
				"run.workspace (preferred and authoritative when both locations are present)",
				`run: {command: ["go", "test", "./..."], workspace: repo-readonly}`,
				"Task-level workspace is also supported as a fallback",
			},
		},
		{
			name: "agentic task",
			def: Definition{Name: "review", Spec: apiv1.WorkflowSpec{Tasks: []apiv1.Task{{
				Name: "review-code", Type: apiv1.TaskAgentic,
			}}}},
			want: true,
			wantText: []string{
				"agentic task",
				"task-level workspace",
				"workspace: repo-readonly",
			},
		},
		{
			name: "agentic gate",
			def: Definition{Name: "merge-review", Spec: apiv1.WorkflowSpec{Gates: []apiv1.Gate{{
				Name: "review", Evaluator: apiv1.EvaluatorAgentic,
				Agentic: &apiv1.AgenticGate{Goober: "reviewer"},
			}}}},
			want: true,
			wantText: []string{
				"agentic gate",
				"agentic.workspace",
				"agentic: {goober: reviewer, workspace: repo-readonly}",
			},
		},
		{
			name: "agentic gate with mutating reviewer",
			def: Definition{Name: "merge-review", Spec: apiv1.WorkflowSpec{Gates: []apiv1.Gate{{
				Name: "review", Evaluator: apiv1.EvaluatorAgentic,
				Agentic: &apiv1.AgenticGate{Goober: "reviewer"},
			}}}},
			goobers: map[string]apiv1.GooberSpec{
				"reviewer": {Capabilities: []string{"repo:push"}},
			},
			want: false,
		},
		{
			name: "git command is uncertain",
			def: Definition{Name: "git-workflow", Spec: apiv1.WorkflowSpec{Tasks: []apiv1.Task{{
				Name: "git", Type: apiv1.TaskDeterministic,
				Run: &apiv1.DeterministicRun{Command: []string{"git", "status"}},
			}}}},
			want: false,
		},
		{
			name: "gofmt write is mutating",
			def: Definition{Name: "format", Spec: apiv1.WorkflowSpec{Tasks: []apiv1.Task{{
				Name: "format", Type: apiv1.TaskDeterministic,
				Run: &apiv1.DeterministicRun{Command: []string{"gofmt", "-w", "."}},
			}}}},
			want: false,
		},
		{
			name: "go generate is mutating",
			def: Definition{Name: "generate", Spec: apiv1.WorkflowSpec{Tasks: []apiv1.Task{{
				Name: "generate", Type: apiv1.TaskDeterministic,
				Run: &apiv1.DeterministicRun{Command: []string{"go", "generate", "./..."}},
			}}}},
			want: false,
		},
		{
			name: "goobers mutation is uncertain",
			def: Definition{Name: "maintain", Spec: apiv1.WorkflowSpec{Tasks: []apiv1.Task{{
				Name: "reset", Type: apiv1.TaskDeterministic,
				Run: &apiv1.DeterministicRun{Command: []string{"goobers", "workspace", "reset"}},
			}}}},
			want: false,
		},
		{
			name: "agentic task policy action",
			def: Definition{Name: "docs", Spec: apiv1.WorkflowSpec{Tasks: []apiv1.Task{{
				Name: "update", Type: apiv1.TaskAgentic,
				PolicyActions: []string{"modify-repository"},
			}}}},
			want: false,
		},
		{
			name: "agentic task goober policy action",
			def: Definition{Name: "docs", Spec: apiv1.WorkflowSpec{Tasks: []apiv1.Task{{
				Name: "update", Type: apiv1.TaskAgentic, Goober: "writer",
			}}}},
			goobers: map[string]apiv1.GooberSpec{
				"writer": {PolicyActions: []string{"modify-repository"}},
			},
			want: false,
		},
		{
			name: "agentic gate policy action",
			def: Definition{Name: "review", Spec: apiv1.WorkflowSpec{Gates: []apiv1.Gate{{
				Name: "review", Evaluator: apiv1.EvaluatorAgentic,
				Agentic: &apiv1.AgenticGate{Goober: "reviewer"},
			}}}},
			goobers: map[string]apiv1.GooberSpec{
				"reviewer": {PolicyActions: []string{"modify-repository"}},
			},
			want: false,
		},
		{
			name: "explicit workspace",
			def: Definition{Name: "declared", Spec: apiv1.WorkflowSpec{Tasks: []apiv1.Task{{
				Name: "tests", Type: apiv1.TaskDeterministic,
				Run: &apiv1.DeterministicRun{
					Command:   []string{"go", "test", "./..."},
					Workspace: apiv1.WorkspaceScratch,
				},
			}}}},
			want: false,
		},
		{
			name: "mutating capability",
			def: Definition{Name: "publish", Spec: apiv1.WorkflowSpec{Tasks: []apiv1.Task{{
				Name: "publish", Type: apiv1.TaskAgentic,
				Capabilities: []string{"repo:push"},
			}}}},
			want: false,
		},
		{
			name: "uncertain deterministic command",
			def: Definition{Name: "custom", Spec: apiv1.WorkflowSpec{Tasks: []apiv1.Task{{
				Name: "custom", Type: apiv1.TaskDeterministic,
				Run: &apiv1.DeterministicRun{Script: "do-whatever"},
			}}}},
			want: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var found bool
			warnings := CheckImplicitWritableWorkspaceWarnings(test.def, test.goobers)
			for _, warning := range warnings {
				if strings.Contains(warning, "omits ") {
					found = true
					if !strings.Contains(warning, `workflow "`+test.def.Name+`"`) {
						t.Fatalf("warning = %q, missing workflow name", warning)
					}
					for _, want := range test.wantText {
						if !strings.Contains(warning, want) {
							t.Fatalf("warning = %q, missing stage-specific guidance %q", warning, want)
						}
					}
					for _, want := range []string{"Choose scratch", "repo-readonly", "repo when writable"} {
						if !strings.Contains(warning, want) {
							t.Fatalf("warning = %q, missing workspace choice %q", warning, want)
						}
					}
				}
			}
			if found != test.want {
				t.Fatalf("implicit workspace warning = %v, want %v; warnings = %v", found, test.want, warnings)
			}
		})
	}
}
