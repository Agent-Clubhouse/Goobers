package main

import (
	"context"
	"sync"
	"testing"

	"github.com/goobers/goobers/internal/instance"
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
	root := initDeterministicDemo(t)
	l := instance.NewLayout(root)

	cfg, err := instance.LoadConfig(l.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	cfg.RunConditions = instance.RunConditions{
		MaxParallelRuns: 3,
		WorkflowBudgets: map[string]int{"default-implement": 7},
	}
	if err := instance.WriteConfig(l.ConfigFile(), cfg); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	setup, err := buildSchedulerSetup(context.Background(), l, &wg)
	if err != nil {
		t.Fatal(err)
	}

	if setup.RunConditions.MaxParallelRuns != 3 {
		t.Fatalf("RunConditions.MaxParallelRuns = %d, want 3", setup.RunConditions.MaxParallelRuns)
	}
	if got := setup.RunConditions.WorkflowBudgets["default-implement"]; got != 7 {
		t.Fatalf("RunConditions.WorkflowBudgets[default-implement] = %d, want 7", got)
	}
}
