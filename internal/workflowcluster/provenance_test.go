package workflowcluster

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestConfiguredStrataExtractsIdentifiersAndDigests(t *testing.T) {
	workflows := []apiv1.Workflow{{
		ObjectMeta: metav1.ObjectMeta{Name: "ship"},
		Spec: apiv1.WorkflowSpec{
			Gaggle: "core",
			Tasks:  []apiv1.Task{{Name: "implement", Type: apiv1.TaskAgentic, Goober: "coder"}},
			Gates:  []apiv1.Gate{{Name: "review", Agentic: &apiv1.AgenticGate{Goober: "reviewer"}}},
		},
	}}
	goobers := []apiv1.Goober{
		{ObjectMeta: metav1.ObjectMeta{Name: "coder"}, Spec: apiv1.GooberSpec{
			Harness: apiv1.HarnessCopilot, Model: "gpt-5.6-sol", Skills: []string{"go", "tests"},
			Tools: []string{"shell"}, MCPServers: []apiv1.MCPServer{{Name: "github"}},
		}},
		{ObjectMeta: metav1.ObjectMeta{Name: "reviewer"}, Spec: apiv1.GooberSpec{
			Harness: apiv1.HarnessCopilot, Model: "gpt-5.6-sol", Skills: []string{"review"},
		}},
	}

	got := configuredStrata(workflows, goobers)
	id := "core/ship"
	if got[StratifyGaggle][id] != "core" || got[StratifyModel][id] != "gpt-5.6-sol" ||
		got[StratifyHarness][id] != "copilot" {
		t.Fatalf("identifier strata = %+v", got)
	}
	for _, axis := range []string{StratifySkills, StratifyTools} {
		if !strings.HasPrefix(got[axis][id], "sha256:") {
			t.Errorf("%s stratum = %q, want digest", axis, got[axis][id])
		}
	}
}

func TestConfiguredStrataCountsUndefinedModelProvenance(t *testing.T) {
	workflows := []apiv1.Workflow{{
		ObjectMeta: metav1.ObjectMeta{Name: "wf"},
		Spec:       apiv1.WorkflowSpec{Gaggle: "g", Tasks: []apiv1.Task{{Goober: "coder"}}},
	}}
	goobers := []apiv1.Goober{{
		ObjectMeta: metav1.ObjectMeta{Name: "coder"},
		Spec:       apiv1.GooberSpec{Harness: apiv1.HarnessCopilot},
	}}
	got := configuredStrata(workflows, goobers)
	if _, ok := got[StratifyModel]["g/wf"]; ok {
		t.Fatal("undefined model was labeled")
	}
	if got[StratifyHarness]["g/wf"] != "copilot" {
		t.Fatalf("harness = %q", got[StratifyHarness]["g/wf"])
	}
}
