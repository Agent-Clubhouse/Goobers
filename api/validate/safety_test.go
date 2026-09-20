package validate

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/workflowsafety"
)

func TestSafetyCompiledValidationBothDSLVersions(t *testing.T) {
	for _, version := range []string{"2.0", "3.0"} {
		t.Run(version, func(t *testing.T) {
			ix := newIndex()
			ix.gaggles["example"] = apiv1.Gaggle{Spec: apiv1.GaggleSpec{}}
			w := apiv1.Workflow{
				ObjectMeta: metav1.ObjectMeta{Name: "example-workflow", Annotations: map[string]string{
					"goobers.dev/allow-preview-features": "true",
				}},
				DSLVersion: version,
				Spec: apiv1.WorkflowSpec{Gaggle: "example", Start: "custom",
					Tasks: []apiv1.Task{{Name: "custom", Type: apiv1.TaskDeterministic,
						Goal: "Test", Run: &apiv1.DeterministicRun{Command: []string{"never-execute-this-command"}}}},
				},
			}
			ix.workflows[workflowIdentity{"example", w.Name}] = indexedWorkflow{
				definition: w, file: "workflow.yaml", lineOffset: 10,
				node: parseYAMLNode("spec:\n  tasks:\n    - name: custom\n"),
			}
			report := &Report{}
			ix.checkWorkflowsCompile(report)
			if report.HasErrors() {
				t.Fatal(joinIssues(report))
			}
			if len(report.Issues) != 1 {
				t.Fatalf("expected one advisory: %+v", report.Issues)
			}
			issue := report.Issues[0]
			if issue.Code != workflowsafety.CoverageCode || issue.Severity != Warning ||
				issue.Safety == nil || issue.Line != 13 || issue.Col == 0 ||
				issue.Gaggle != "example" || !strings.Contains(issue.Message, "Advisory only") {
				t.Fatalf("missing advisory provenance: %+v", issue)
			}
			if issue.Safety.File != issue.File || issue.Safety.Line != issue.Line || issue.Safety.Col != issue.Col {
				t.Fatalf("persistent advisory lost YAML source location: %+v", issue)
			}
			if warnings := report.Warnings(); len(warnings) != 1 || warnings[0].Safety.ID != issue.Safety.ID {
				t.Fatalf("persistent warnings lost safety details: %+v", warnings)
			}
		})
	}
}

func TestSafetyDoesNotChangeExistingCompileFailures(t *testing.T) {
	ix := newIndex()
	w := apiv1.Workflow{ObjectMeta: metav1.ObjectMeta{Name: "broken"}, DSLVersion: "2.0",
		Spec: apiv1.WorkflowSpec{Gaggle: "example", Start: "missing"}}
	ix.workflows[workflowIdentity{"example", w.Name}] = indexedWorkflow{definition: w, file: "workflow.yaml"}
	report := &Report{}
	ix.checkWorkflowsCompile(report)
	if !report.HasErrors() || len(report.Issues) != 1 || report.Issues[0].Code != errorWorkflowCompile {
		t.Fatalf("compile behavior changed: %+v", report)
	}
}
