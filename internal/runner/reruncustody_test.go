package runner

import (
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
	"testing"
)

func TestRerunWritableHistoryExcludesNonAgenticGates(t *testing.T) {
	for _, evaluator := range []apiv1.EvaluatorKind{apiv1.EvaluatorAutomated, apiv1.EvaluatorHuman, apiv1.EvaluatorAgentic} {
		t.Run(string(evaluator), func(t *testing.T) {
			gate := apiv1.Gate{Name: "check", Evaluator: evaluator, Branches: map[string]string{"pass": "work", "fail": workflow.TargetEscalate}}
			switch evaluator {
			case apiv1.EvaluatorAgentic:
				gate.Agentic = &apiv1.AgenticGate{Goober: "reviewer"}
				gate.Branches["needs-changes"] = workflow.TargetEscalate
			case apiv1.EvaluatorHuman:
				gate.Human = &apiv1.HumanGate{}
			default:
				gate.Automated = &apiv1.AutomatedGate{Check: "status-equals"}
			}
			machine, err := workflow.Compile(workflow.Definition{Name: "history", Version: 1, Spec: apiv1.WorkflowSpec{Gaggle: "example", Start: "check", Tasks: []apiv1.Task{{Name: "work", Type: apiv1.TaskAgentic, Goal: "work", Goober: "coder", Workspace: apiv1.WorkspaceScratch, Next: workflow.TerminalComplete}}, Gates: []apiv1.Gate{gate}}}, workflow.WithPreviewFeatures(true))
			if err != nil {
				t.Fatal(err)
			}
			got := rerunRequiresWritableWorkspace(machine, []journal.Event{{Type: journal.EventGateStarted, Gate: "check"}}, "work")
			if got != (evaluator == apiv1.EvaluatorAgentic) {
				t.Fatalf("writable history=%v for %s", got, evaluator)
			}
		})
	}
}
