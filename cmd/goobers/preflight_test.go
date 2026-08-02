package main

import (
	"context"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/harness"
)

// harnessFakeRunner is a ProcessRunner double reporting a fixed exit code, used
// to drive a real CopilotAdapter's Preflight to success/failure without a real
// CLI subprocess.
type harnessFakeRunner struct{ exit int }

func (r *harnessFakeRunner) Run(context.Context, harness.ProcessRequest) (harness.ProcessResult, error) {
	return harness.ProcessResult{ExitCode: r.exit, Transcript: []byte("copilot version 1.2.3\n")}, nil
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
	if _, err := preflightAgenticHarnesses(goobers, agentic); err == nil {
		t.Fatal("expected preflight to fail closed on an unusable agentic harness")
	}
	// A deterministic-only workflow references no harness, so it must not be
	// gated by a broken harness (the adapter would fail if consulted).
	if _, err := preflightAgenticHarnesses(goobers, deterministicOnly); err != nil {
		t.Fatalf("deterministic-only workflow must not preflight a harness: %v", err)
	}

	// Healthy harness → preflight passes.
	harnessAdapterFor = func(apiv1.Harness) (harness.Adapter, error) {
		return &harness.CopilotAdapter{Command: []string{"echo"}, Runner: &harnessFakeRunner{exit: 0}}, nil
	}
	info, err := preflightAgenticHarnesses(goobers, agentic)
	if err != nil {
		t.Fatalf("healthy agentic harness should preflight OK: %v", err)
	}
	if got := info[apiv1.HarnessCopilot].Version; got != "copilot version 1.2.3" {
		t.Fatalf("preflight version = %q", got)
	}

	gateOnly := []apiv1.Workflow{{Spec: apiv1.WorkflowSpec{Gates: []apiv1.Gate{{
		Name: "review", Evaluator: apiv1.EvaluatorAgentic,
		Agentic: &apiv1.AgenticGate{Goober: "reviewer"},
	}}}}}
	info, err = preflightAgenticHarnesses(
		map[string]apiv1.GooberSpec{"reviewer": {}},
		gateOnly,
	)
	if err != nil {
		t.Fatalf("reviewer-only default harness preflight: %v", err)
	}
	if got := info[apiv1.HarnessCopilot].Version; got != "copilot version 1.2.3" {
		t.Fatalf("reviewer preflight version = %q", got)
	}
}

// TestAdapterForConfiguresAuthProbe proves the #238 wiring: the default
// CopilotAdapter carries the auth probe (copilotAuthCheckArgs), so every
// preflight through adapterFor — validate --check-harness AND the automatic
// daemon-startup preflight — verifies sign-in, not just CLI presence.
func TestAdapterForConfiguresAuthProbe(t *testing.T) {
	a, err := adapterFor(apiv1.HarnessCopilot)
	if err != nil {
		t.Fatalf("adapterFor(copilot): %v", err)
	}
	ca, ok := a.(*harness.CopilotAdapter)
	if !ok {
		t.Fatalf("adapterFor returned %T, want *harness.CopilotAdapter", a)
	}
	if len(ca.AuthCheckArgs) == 0 {
		t.Fatal("adapterFor's CopilotAdapter has no AuthCheckArgs — the daemon-startup preflight would skip the sign-in probe (#238)")
	}
	if strings.Join(ca.AuthCheckArgs, " ") != strings.Join(copilotAuthCheckArgs, " ") {
		t.Fatalf("AuthCheckArgs = %v, want the confirmed probe %v", ca.AuthCheckArgs, copilotAuthCheckArgs)
	}
}

// TestPreflightAgenticHarnessesCatchesSignedOut is #238 AC3: a harness that is
// installed (--version exits 0) but signed out (the auth probe exits non-zero)
// now fails the automatic daemon-startup preflight — the #284 incident caught
// at startup instead of as a burned mid-run agentic attempt. Before #238 the
// startup path ran only the version check, so this signed-out harness passed
// preflight and failed later, mid-run.
func TestPreflightAgenticHarnessesCatchesSignedOut(t *testing.T) {
	orig := harnessAdapterFor
	t.Cleanup(func() { harnessAdapterFor = orig })

	goobers := map[string]apiv1.GooberSpec{"nominator": {Harness: apiv1.HarnessCopilot}}
	agentic := []apiv1.Workflow{{Spec: apiv1.WorkflowSpec{Tasks: []apiv1.Task{
		{Name: "nominate", Type: apiv1.TaskAgentic, Goober: "nominator"},
	}}}}

	// Installed but signed out: version 0, auth probe non-zero. The adapter
	// carries copilotAuthCheckArgs (as the real adapterFor now does), so the
	// probe actually runs during the startup preflight.
	harnessAdapterFor = func(apiv1.Harness) (harness.Adapter, error) {
		return &harness.CopilotAdapter{
			Command:       []string{"echo"},
			AuthCheckArgs: copilotAuthCheckArgs,
			Runner:        &authProbeFakeRunner{versionExit: 0, authExit: 1},
		}, nil
	}
	_, err := preflightAgenticHarnesses(goobers, agentic)
	if err == nil {
		t.Fatal("expected the daemon-startup preflight to fail closed on a signed-out harness")
	}
	if !strings.Contains(err.Error(), "sign-in check") {
		t.Fatalf("err = %v, want it to mention the sign-in check (the auth probe, not the version check)", err)
	}
}
