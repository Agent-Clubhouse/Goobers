package main

import (
	"slices"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/harness"
)

func TestLauncherContractRequiredThroughProductionAdapterLookup(t *testing.T) {
	for _, tc := range []struct {
		name     string
		commands map[string][]string
		custom   bool
	}{
		{name: "default"},
		{name: "explicit direct", commands: map[string][]string{"copilot": {"copilot"}}},
		{name: "forwarding launcher", commands: map[string][]string{"copilot": {"launcher", "copilot"}}, custom: true},
		{name: "renamed executable", commands: map[string][]string{"copilot": {"my-copilot"}}, custom: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			adapter, err := adapterFor(apiv1.HarnessCopilot, nil, tc.commands, nil)
			if err != nil {
				t.Fatal(err)
			}
			copilot := adapter.(*harness.CopilotAdapter)
			if copilot.RequireLauncherContract != tc.custom {
				t.Fatalf("contract required=%v, want %v", copilot.RequireLauncherContract, tc.custom)
			}
			if copilot.AllowAdapterManagedFallback != tc.custom {
				t.Fatalf("adapter-managed fallback=%v, want %v", copilot.AllowAdapterManagedFallback, tc.custom)
			}
			if copilot.VerifyAdapterManagedSession != tc.custom {
				t.Fatalf("adapter-managed session verification=%v, want %v", copilot.VerifyAdapterManagedSession, tc.custom)
			}
			if copilot.DisableUsageOutput != tc.custom {
				t.Fatalf("conservative usage capture=%v, want %v", copilot.DisableUsageOutput, tc.custom)
			}
			if got := len(copilot.RequiredTools) == 1 && copilot.RequiredTools[0] == "task_complete"; got != tc.custom {
				t.Fatalf("launcher required tool configured=%v, want %v: %v", got, tc.custom, copilot.RequiredTools)
			}
			if got := slices.Contains(copilot.AuthCheckArgs, "--available-tools=view,task_complete"); got != tc.custom {
				t.Fatalf("launcher auth completion tool configured=%v, want %v: %v", got, tc.custom, copilot.AuthCheckArgs)
			}
			if tc.custom {
				for _, arg := range []string{"--silent", "--no-ask-user", "--autopilot"} {
					if !slices.Contains(copilot.AuthCheckArgs, arg) {
						t.Fatalf("launcher auth probe missing %q: %v", arg, copilot.AuthCheckArgs)
					}
				}
			}
		})
	}
}
