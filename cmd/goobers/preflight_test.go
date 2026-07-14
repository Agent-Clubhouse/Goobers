package main

import (
	"context"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/harness"
)

// harnessFakeRunner is a ProcessRunner double reporting a fixed exit code, used
// to drive a real CopilotAdapter's Preflight to success/failure without a real
// CLI subprocess.
type harnessFakeRunner struct{ exit int }

func (r *harnessFakeRunner) Run(context.Context, harness.ProcessRequest) (harness.ProcessResult, error) {
	return harness.ProcessResult{ExitCode: r.exit}, nil
}

// TestPreflightAgenticHarnesses is the #238 control: an agentic stage's unusable
// harness fails preflight (fail closed), a healthy one passes, and a
// deterministic-only workflow preflights no harness at all.
func TestPreflightAgenticHarnesses(t *testing.T) {
	orig := harnessAdapterFor
	t.Cleanup(func() { harnessAdapterFor = orig })

	goobers := map[string]apiv1.GooberSpec{"nominator": {Harness: apiv1.HarnessCopilot}}
	agentic := []apiv1.Workflow{{Spec: apiv1.WorkflowSpec{Tasks: []apiv1.Task{
		{Name: "nominate", Type: apiv1.TaskAgentic, Goober: "nominator"},
	}}}}
	deterministicOnly := []apiv1.Workflow{{Spec: apiv1.WorkflowSpec{Tasks: []apiv1.Task{
		{Name: "gather", Type: apiv1.TaskDeterministic},
	}}}}

	// Unusable harness (its version check exits non-zero) → fail closed.
	harnessAdapterFor = func(apiv1.Harness) (harness.Adapter, error) {
		return &harness.CopilotAdapter{Command: []string{"echo"}, Runner: &harnessFakeRunner{exit: 1}}, nil
	}
	if err := preflightAgenticHarnesses(goobers, agentic); err == nil {
		t.Fatal("expected preflight to fail closed on an unusable agentic harness")
	}
	// A deterministic-only workflow references no harness, so it must not be
	// gated by a broken harness (the adapter would fail if consulted).
	if err := preflightAgenticHarnesses(goobers, deterministicOnly); err != nil {
		t.Fatalf("deterministic-only workflow must not preflight a harness: %v", err)
	}

	// Healthy harness → preflight passes.
	harnessAdapterFor = func(apiv1.Harness) (harness.Adapter, error) {
		return &harness.CopilotAdapter{Command: []string{"echo"}, Runner: &harnessFakeRunner{exit: 0}}, nil
	}
	if err := preflightAgenticHarnesses(goobers, agentic); err != nil {
		t.Fatalf("healthy agentic harness should preflight OK: %v", err)
	}
}
