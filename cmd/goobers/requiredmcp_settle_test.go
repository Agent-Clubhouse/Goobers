package main

import (
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/agentickit"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
)

// #5397: runner.requiredMCPSettleTimeout reaches the daemon's Copilot adapter,
// and an unset value leaves the adapter on its own default.
func TestRequiredMCPSettleTimeoutReachesDaemonCopilotAdapter(t *testing.T) {
	for value, want := range map[string]time.Duration{"": 0, "2m": 2 * time.Minute} {
		registry, err := buildHarnessRegistry(nil, harnessEnvironmentPolicy(instance.RunnerConfig{RequiredMCPSettleTimeout: value}), nil, "", "", true, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		adapter, err := registry.Get(string(apiv1.HarnessCopilot))
		if err != nil {
			t.Fatal(err)
		}
		copilot, ok := adapter.(*harness.CopilotAdapter)
		if !ok || copilot.RequiredMCPSettleTimeout != want {
			t.Fatalf("settle %q: adapter=%#v want %s", value, adapter, want)
		}
	}
}

// A legacy kit, which carries no settle budget, keeps the adapter default.
func TestPodHarnessEnvironmentSettleTimeout(t *testing.T) {
	for value, want := range map[string]time.Duration{"": 0, "45s": 45 * time.Second} {
		got := podHarnessEnvironment(&agentickit.Kit{RequiredMCPSettleTimeout: value}, apiv1.HarnessCopilot).RequiredMCPSettleTimeout
		if got != want {
			t.Fatalf("kit settle %q: got %s want %s", value, got, want)
		}
	}
}
