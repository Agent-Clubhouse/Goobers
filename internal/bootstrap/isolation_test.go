package bootstrap

import (
	"reflect"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
)

func TestPinIsolationMandatesRefuseUnprotectedStagesAndUnplacedReviewers(t *testing.T) {
	cfg := distributedConfig()
	def := placementSpec(apiv1.Task{Name: "implement", Type: apiv1.TaskAgentic, Goober: "dev"})
	before, err := PinStagePlacements(cfg, placementConfigSet(), "web", def)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Isolation = &instance.IsolationConfig{Mandates: []instance.IsolationMandate{{Match: instance.IsolationMatch{StageClass: "agentic"}, Restrictions: []instance.RunnerRestriction{instance.RunnerRestrictionTmpEphemeral}}}}
	_, err = PinStagePlacements(cfg, placementConfigSet(), "web", def)
	if err == nil || !strings.Contains(err.Error(), "implement") || !strings.Contains(err.Error(), "tmp:ephemeral") {
		t.Fatalf("unprotected pin: %v", err)
	}
	cfg.Runners[1].Restrictions = []instance.RunnerRestriction{instance.RunnerRestrictionTmpEphemeral}
	pins, err := PinStagePlacements(cfg, placementConfigSet(), "web", def)
	if err != nil || len(pins) != 1 || pins[0].Self {
		t.Fatalf("protected pin: %+v, %v", pins, err)
	}
	def.Spec.Gates = []apiv1.Gate{{Name: "review", Evaluator: apiv1.EvaluatorAgentic, Agentic: &apiv1.AgenticGate{Goober: "reviewer"}}}
	_, err = PinStagePlacements(cfg, placementConfigSet(), "web", def)
	if err == nil || !strings.Contains(err.Error(), "review") {
		t.Fatalf("unplaced reviewer escaped floor: %v", err)
	}
	def.Spec.Gates = nil
	cfg.Isolation = nil
	cfg.Runners[1].Restrictions = nil
	after, err := PinStagePlacements(cfg, placementConfigSet(), "web", def)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("absent policy changed pin: %+v, %v", after, err)
	}
}

func TestPinIsolationMandatesDoNotSkipLocalInventories(t *testing.T) {
	def := placementSpec(apiv1.Task{Name: "agent", Type: apiv1.TaskAgentic})
	cfg := &instance.Config{Isolation: &instance.IsolationConfig{Mandates: []instance.IsolationMandate{{Match: instance.IsolationMatch{StageClass: "agentic"}, Restrictions: []instance.RunnerRestriction{instance.RunnerRestrictionTmpEphemeral}}}}}
	for _, runners := range [][]instance.RunnerEntry{nil, {{Name: "self", Host: "self"}}} {
		cfg.Runners = runners
		_, err := PinStagePlacements(cfg, placementConfigSet(), "web", def)
		if err == nil || !strings.Contains(err.Error(), "tmp:ephemeral") {
			t.Fatalf("local inventory skipped floor: %v", err)
		}
	}
}
