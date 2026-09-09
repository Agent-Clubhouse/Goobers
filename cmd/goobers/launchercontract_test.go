package main

import (
	"context"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/harness"
)

type incompatibleLauncherRunner struct{ calls int }

func (r *incompatibleLauncherRunner) Run(_ context.Context, req harness.ProcessRequest) (harness.ProcessResult, error) {
	r.calls++
	return harness.ProcessResult{ExitCode: 2}, nil
}

func TestLauncherContractRequiredThroughProductionAdapterLookup(t *testing.T) {
	for _, tc := range []struct {
		name     string
		commands map[string][]string
		required bool
	}{
		{name: "default"},
		{name: "explicit direct", commands: map[string][]string{"copilot": {"copilot"}}},
		{name: "wrapper", commands: map[string][]string{"copilot": {"agency", "copilot"}}, required: true},
		{name: "renamed executable", commands: map[string][]string{"copilot": {"my-copilot"}}, required: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			adapter, err := adapterFor(apiv1.HarnessCopilot, nil, tc.commands, nil)
			if err != nil {
				t.Fatal(err)
			}
			copilot := adapter.(*harness.CopilotAdapter)
			if copilot.RequireLauncherContract != tc.required {
				t.Fatalf("contract required=%v, want %v", copilot.RequireLauncherContract, tc.required)
			}
			if tc.required {
				process := &incompatibleLauncherRunner{}
				copilot.Runner = process
				if _, err := copilot.Preflight(context.Background()); err == nil || !strings.Contains(err.Error(), "incompatible") {
					t.Fatalf("preflight accepted incompatible wrapper: %v", err)
				}
				if process.calls != 1 {
					t.Fatalf("incompatible wrapper reached subsequent probes: %d", process.calls)
				}
			}
		})
	}
}
