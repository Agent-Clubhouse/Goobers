package main

import (
	"context"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
)

// TestBuildSchedulerSetupCarriesInstanceRunConditions is issue #142:
// instance.yaml's runConditions (maxParallelRuns/workflowBudgets) were
// parsed and scaffolded but never reached the scheduler that's supposed to
// enforce them (up.go/run.go now pass
// localscheduler.WithInstanceRunConditions(setup.RunConditions...)). This
// proves buildSchedulerSetup — the shared construction path both commands
// use — actually carries a configured value through rather than dropping it,
// the gap that made the rest of the wiring moot.
func TestBuildSchedulerSetupCarriesInstanceRunConditions(t *testing.T) {
	root := t.TempDir()
	if _, err := instance.Init(root); err != nil {
		t.Fatal(err)
	}
	l := instance.NewLayout(root)

	cfg, err := instance.LoadConfig(l.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	cfg.RunConditions = instance.RunConditions{
		MaxParallelRuns:       3,
		WorkflowBudgets:       map[string]int{"default-implement": 7},
		DisableCadenceBudgets: true,
	}
	if err := instance.WriteConfig(l.ConfigFile(), cfg); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	setup, err := buildSchedulerSetup(context.Background(), l, &wg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = setup.Shutdown(context.Background()) }()

	if setup.RunConditions.MaxParallelRuns != 3 {
		t.Fatalf("RunConditions.MaxParallelRuns = %d, want 3", setup.RunConditions.MaxParallelRuns)
	}
	if got := setup.RunConditions.WorkflowBudgets["default-implement"]; got != 7 {
		t.Fatalf("RunConditions.WorkflowBudgets[default-implement] = %d, want 7", got)
	}
	starter := &acceptedRunIDStarter{ids: make(chan string, 12)}
	scheduler := localscheduler.New([]localscheduler.WorkflowEntry{{
		Workflow:  "wf",
		Readiness: apiv1.ReadinessConditions{MaxRunsPerDay: 2},
		Starter:   starter,
	}}, nil, setup.SchedulerOptions()...)
	for i := 0; i < 12; i++ {
		if _, err := scheduler.TriggerExact(t.Context(), localscheduler.WorkflowIdentity{Workflow: "wf"}, time.Now()); err != nil {
			t.Fatalf("unthrottled trigger %d through shared setup: %v", i, err)
		}
		scheduler.Wait()
	}
	if got := len(starter.ids); got != 12 {
		t.Fatalf("starts = %d, want 12 beyond default hourly and explicit daily ceilings", got)
	}
}
