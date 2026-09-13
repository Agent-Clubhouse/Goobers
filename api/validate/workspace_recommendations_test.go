package validate

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	wf "github.com/goobers/goobers/internal/workflow"
)

func TestWS001SuggestedYAMLShapesValidateAgainstGeneratedSchema(t *testing.T) {
	v := newV(t)
	stages := []struct {
		name     string
		start    string
		yaml     string
		def      wf.Definition
		wantText string
	}{
		{
			name:  "agentic task task-level workspace",
			start: "review-code",
			yaml: `tasks:
    - name: review-code
      type: agentic
      goal: Review the code.
      goober: reviewer
      workspace: repo-readonly`,
			def: wf.Definition{Name: "workspace-recommendation", Spec: apiv1.WorkflowSpec{Tasks: []apiv1.Task{{
				Name: "review-code", Type: apiv1.TaskAgentic,
			}}}},
			wantText: "workspace: repo-readonly",
		},
		{
			name:  "deterministic task run workspace",
			start: "tests",
			yaml: `tasks:
    - name: tests
      type: deterministic
      goal: Run the tests.
      run: {command: ["go", "test", "./..."], workspace: repo-readonly}`,
			def: wf.Definition{Name: "workspace-recommendation", Spec: apiv1.WorkflowSpec{Tasks: []apiv1.Task{{
				Name: "tests", Type: apiv1.TaskDeterministic,
				Run: &apiv1.DeterministicRun{Command: []string{"go", "test", "./..."}},
			}}}},
			wantText: `run: {command: ["go", "test", "./..."], workspace: repo-readonly}`,
		},
		{
			name:  "agentic gate nested workspace",
			start: "prepare",
			yaml: `tasks:
    - name: prepare
      type: deterministic
      goal: Prepare the review.
      run: {command: ["true"]}
      next: review
  gates:
    - name: review
      evaluator: agentic
      agentic: {goober: reviewer, workspace: repo-readonly}
      branches: {pass: "", fail: "@abort"}`,
			def: wf.Definition{Name: "workspace-recommendation", Spec: apiv1.WorkflowSpec{Gates: []apiv1.Gate{{
				Name: "review", Evaluator: apiv1.EvaluatorAgentic,
				Agentic: &apiv1.AgenticGate{Goober: "reviewer"},
			}}}},
			wantText: "agentic: {goober: reviewer, workspace: repo-readonly}",
		},
	}

	for _, version := range []string{"2.0", "3.0"} {
		for _, stage := range stages {
			t.Run(version+"/"+stage.name, func(t *testing.T) {
				warnings := wf.CheckImplicitWritableWorkspaceWarnings(stage.def)
				if len(warnings) != 1 || !strings.Contains(warnings[0], stage.wantText) {
					t.Fatalf("WS001 guidance does not contain tested YAML shape %q: %v", stage.wantText, warnings)
				}
				doc := `apiVersion: goobers.dev/v1alpha1
kind: Workflow
metadata:
  name: workspace-recommendation
dslVersion: "` + version + `"
spec:
  gaggle: example
  triggers:
    - type: manual
  start: ` + stage.start + `
  ` + stage.yaml + `
`
				jsonDoc, err := yaml.YAMLToJSON([]byte(doc))
				if err != nil {
					t.Fatalf("convert suggested YAML: %v\n%s", err, doc)
				}
				if err := v.ValidateJSON("workflow.schema.json", jsonDoc); err != nil {
					t.Fatalf("suggested YAML does not validate against workflow.schema.json: %v\n%s", err, doc)
				}
			})
		}
	}
}
