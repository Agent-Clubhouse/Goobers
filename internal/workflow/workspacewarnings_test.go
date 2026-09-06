package workflow

import (
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestImplicitWritableWorkspaceWarnings(t *testing.T) {
	tests := []struct {
		name    string
		def     Definition
		goobers map[string]apiv1.GooberSpec
		want    bool
	}{
		{
			name: "read-only deterministic command",
			def: Definition{Name: "inspect", Spec: apiv1.WorkflowSpec{Tasks: []apiv1.Task{{
				Name: "tests", Type: apiv1.TaskDeterministic,
				Run: &apiv1.DeterministicRun{Command: []string{"go", "test", "./..."}},
			}}}},
			want: true,
		},
		{
			name: "agentic task",
			def: Definition{Name: "review", Spec: apiv1.WorkflowSpec{Tasks: []apiv1.Task{{
				Name: "review-code", Type: apiv1.TaskAgentic,
			}}}},
			want: true,
		},
		{
			name: "agentic gate",
			def: Definition{Name: "merge-review", Spec: apiv1.WorkflowSpec{Gates: []apiv1.Gate{{
				Name: "review", Evaluator: apiv1.EvaluatorAgentic,
				Agentic: &apiv1.AgenticGate{Goober: "reviewer"},
			}}}},
			want: true,
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
				if strings.Contains(warning, "omits workspace") {
					found = true
					if !strings.Contains(warning, `workflow "`+test.def.Name+`"`) ||
						!strings.Contains(warning, "workspace: scratch") ||
						!strings.Contains(warning, "workspace: repo-readonly") ||
						!strings.Contains(warning, "workspace: repo") {
						t.Fatalf("warning = %q, missing workflow/stage/default/recommendations", warning)
					}
				}
			}
			if found != test.want {
				t.Fatalf("implicit workspace warning = %v, want %v; warnings = %v", found, test.want, warnings)
			}
		})
	}
}
