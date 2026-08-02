package runcontrol

import (
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestResolvePrecedenceAndPartialInheritance(t *testing.T) {
	gaggle := &apiv1.RunControls{MaxRepasses: 5, MaxRunDuration: "8h"}
	workflow := &apiv1.RunControls{StalledRunTimeout: "3h"}
	got, err := Resolve(
		apiv1.RunControls{MaxRepasses: 2, StalledRunTimeout: "30m"},
		gaggle,
		workflow,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxRepasses != 5 || got.StalledRunTimeout != 3*time.Hour || got.MaxRunDuration != 8*time.Hour {
		t.Fatalf("Resolve = %+v, want maxRepasses=5 stalledRunTimeout=3h maxRunDuration=8h", got)
	}
}

func TestResolveDefaults(t *testing.T) {
	got, err := Resolve(apiv1.RunControls{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.MaxRepasses != DefaultMaxRepasses || got.StalledRunTimeout != DefaultStalledRunTimeout || got.MaxRunDuration != 0 {
		t.Fatalf("Resolve defaults = %+v", got)
	}
}

func TestResolveRejectsInvalidDurationAtItsScope(t *testing.T) {
	_, err := Resolve(apiv1.RunControls{}, &apiv1.RunControls{StalledRunTimeout: "later"}, nil)
	if err == nil || !strings.Contains(err.Error(), "gaggle.spec.runControls.stalledRunTimeout") {
		t.Fatalf("Resolve error = %v, want gaggle run-control path", err)
	}
}

func TestResolveRejectsInvalidMaxRunDurationAtItsScope(t *testing.T) {
	_, err := Resolve(apiv1.RunControls{}, nil, &apiv1.RunControls{MaxRunDuration: "forever"})
	if err == nil || !strings.Contains(err.Error(), "workflow.spec.runControls.maxRunDuration") {
		t.Fatalf("Resolve error = %v, want workflow max-run-duration path", err)
	}
}

func TestMaxRepassesForGate(t *testing.T) {
	gate := apiv1.Gate{MaxRepasses: 1}
	if got := MaxRepassesForGate(gate, 7); got != 1 {
		t.Fatalf("MaxRepassesForGate = %d, want gate override 1", got)
	}
	gate.MaxRepasses = 0
	if got := MaxRepassesForGate(gate, 7); got != 7 {
		t.Fatalf("MaxRepassesForGate inherited = %d, want 7", got)
	}
}

func TestValidateWorkflowRejectsHumanGateRepassBudget(t *testing.T) {
	err := ValidateWorkflow(apiv1.WorkflowSpec{Gates: []apiv1.Gate{{
		Name:        "approval",
		Evaluator:   apiv1.EvaluatorHuman,
		MaxRepasses: 1,
	}}})
	if err == nil || !strings.Contains(err.Error(), "only valid for automated or agentic") {
		t.Fatalf("ValidateWorkflow error = %v", err)
	}
}

func TestValidatePinnedRequiresCompletePolicy(t *testing.T) {
	if err := ValidatePinned(nil); err != nil {
		t.Fatalf("legacy nil controls: %v", err)
	}
	if err := ValidatePinned(&apiv1.RunControls{MaxRepasses: 3}); err == nil ||
		!strings.Contains(err.Error(), "stalledRunTimeout is required") {
		t.Fatalf("partial pinned controls error = %v", err)
	}
}
