package startinput

import (
	"errors"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/cloud/acceptance"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestResolvePinsTheEnabledChildPostureByDefault(t *testing.T) {
	f := newFixture(t)
	in, err := f.resolver.Resolve(t.Context(), f.record(t, "gen-1", "implementation", nil))
	if err != nil {
		t.Fatal(err)
	}
	if in.ChildWorkflowsEnabled {
		t.Fatal("mutation expected the enabled child posture pin to be absent")
	}
	f.balanced(t, "default posture")
}

func TestResolvePinsTheDisabledChildPosture(t *testing.T) {
	f := newFixture(t)
	disabled := false
	cfg := *f.cfg
	engineCfg := f.cfg.EffectiveEngineConfig()
	engineCfg.ChildWorkflows = &instance.EngineChildWorkflowsConfig{Enabled: &disabled}
	cfg.Engine = &engineCfg
	f.resolver.Config = func() *instance.Config { return &cfg }
	in, err := f.resolver.Resolve(t.Context(), f.record(t, "gen-1", "implementation", nil))
	if err != nil {
		t.Fatalf("a workflow without children must still start when children are disabled: %v", err)
	}
	if in.ChildWorkflowsEnabled {
		t.Fatal("engine.childWorkflows.enabled=false must pin a disabled posture")
	}
	f.balanced(t, "disabled posture")
}

func TestDisabledChildPostureRefusesChildWorkflowsBeforeAnyStage(t *testing.T) {
	spec := apiv1.WorkflowSpec{Tasks: []apiv1.Task{
		{Name: "plan"},
		{Name: "fanout", ChildWorkflows: &apiv1.ChildWorkflowPolicy{}},
	}}
	err := refuseDisabledChildren(triggerqueue.Accepted, false, spec)
	if !errors.Is(err, ErrChildWorkflowsDisabled) || !errors.Is(err, acceptance.ErrPermanentStart) {
		t.Fatalf("disabled posture refusal = %v; want a permanent ErrChildWorkflowsDisabled", err)
	}
	if err := refuseDisabledChildren(triggerqueue.Dispatching, false, spec); err != nil {
		t.Fatalf("dispatching recovery was refused after posture rollback: %v", err)
	}
	if err := refuseDisabledChildren(triggerqueue.Accepted, true, spec); err != nil {
		t.Fatalf("enabled posture must admit children: %v", err)
	}
	plain := apiv1.WorkflowSpec{Tasks: []apiv1.Task{{Name: "plan"}}}
	if err := refuseDisabledChildren(triggerqueue.Accepted, false, plain); err != nil {
		t.Fatalf("a workflow without children is unaffected by the posture: %v", err)
	}
}
