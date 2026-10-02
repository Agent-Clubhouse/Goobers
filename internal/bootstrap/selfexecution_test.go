package bootstrap

import (
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
)

func TestSelfExecutionDeniedPinning(t *testing.T) {
	cfg := &instance.Config{Placement: &instance.PlacementConfig{SelfExecution: "deny"}}
	def := placementSpec(apiv1.Task{Name: "build", Type: apiv1.TaskDeterministic, Run: &apiv1.DeterministicRun{Command: []string{"true"}}})
	if _, err := PinStagePlacements(cfg, placementConfigSet(), "web", def); err == nil || !strings.Contains(err.Error(), `stage "build"`) {
		t.Fatalf("implicit self bypass: %v", err)
	}
	cfg.Runners = []instance.RunnerEntry{{Name: "daemon-alias", Host: "self"}, {Name: "pod", Host: "example/worker:v1", Provides: instance.RunnerProvides{Shell: true}}}
	pins, err := PinStagePlacements(cfg, placementConfigSet(), "web", def)
	if err != nil || len(pins) != 1 || pins[0].Self || pins[0].Eligible[0].Name != "pod" {
		t.Fatalf("remote pin: %+v %v", pins, err)
	}
	def.Spec.Gates = []apiv1.Gate{{Name: "review", Evaluator: apiv1.EvaluatorAgentic, Agentic: &apiv1.AgenticGate{Goober: "reviewer"}}}
	if _, err = PinStagePlacements(cfg, placementConfigSet(), "web", def); err == nil || !strings.Contains(err.Error(), `stage "review"`) {
		t.Fatalf("unpinned reviewer bypass: %v", err)
	}
}

func TestSelfExecutionDeniedReferenceConfigPlacesWork(t *testing.T) {
	cfg, err := instance.LoadConfig(filepath.Join("..", "..", "deploy", "reference", "instance.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.SelfExecutionDenied() {
		t.Fatal("hosted reference must explicitly deny self")
	}
	def := placementSpec(apiv1.Task{Name: "implement", Type: apiv1.TaskDeterministic, Run: &apiv1.DeterministicRun{Command: []string{"true"}}})
	pins, err := PinStagePlacements(cfg, placementConfigSet(), "web", def)
	if err != nil || len(pins) != 1 || pins[0].Self {
		t.Fatalf("reference config pins %+v %v", pins, err)
	}
}
