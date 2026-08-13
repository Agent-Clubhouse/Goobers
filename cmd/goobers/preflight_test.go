package main

import (
	"context"
	"errors"
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

// TestPreflightAgenticHarnesses is the #238/#2812 control: an agentic stage's
// unusable harness is recorded as a failure (not fatal), a healthy one
// passes, and a deterministic-only workflow preflights no harness at all.
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

	// Unusable harness (its version check exits non-zero) → recorded as a
	// failure, but the call itself does not error (#2812: scoped, not fatal).
	harnessAdapterFor = func(apiv1.Harness, []string, map[string][]string) (harness.Adapter, error) {
		return &harness.CopilotAdapter{Command: []string{"echo"}, Runner: &harnessFakeRunner{exit: 1}}, nil
	}
	info, failures, err := preflightAgenticHarnesses(goobers, agentic, nil, nil)
	if err != nil {
		t.Fatalf("an unusable harness must not fail the whole preflight call: %v", err)
	}
	if _, ok := info[apiv1.HarnessCopilot]; ok {
		t.Fatal("unusable harness must not appear in the success info map")
	}
	if failures[apiv1.HarnessCopilot] == nil {
		t.Fatal("expected the unusable harness to be recorded in failures")
	}

	// A deterministic-only workflow references no harness, so it must not be
	// gated by a broken harness (the adapter would fail if consulted).
	if _, _, err := preflightAgenticHarnesses(goobers, deterministicOnly, nil, nil); err != nil {
		t.Fatalf("deterministic-only workflow must not preflight a harness: %v", err)
	}

	// Healthy harness → preflight passes, no failure recorded.
	var gotEnvPassthrough []string
	harnessAdapterFor = func(_ apiv1.Harness, envPassthrough []string, _ map[string][]string) (harness.Adapter, error) {
		gotEnvPassthrough = append([]string(nil), envPassthrough...)
		return &harness.CopilotAdapter{Command: []string{"echo"}, Runner: &harnessFakeRunner{exit: 0}}, nil
	}
	info, failures, err = preflightAgenticHarnesses(goobers, agentic, []string{"CLAUDE_CONFIG_DIR"}, nil)
	if err != nil {
		t.Fatalf("healthy agentic harness should preflight OK: %v", err)
	}
	if len(failures) != 0 {
		t.Fatalf("healthy harness must not appear in failures: %v", failures)
	}
	if got := info[apiv1.HarnessCopilot].Version; got != "copilot version 1.2.3" {
		t.Fatalf("preflight version = %q", got)
	}
	if strings.Join(gotEnvPassthrough, ",") != "CLAUDE_CONFIG_DIR" {
		t.Fatalf("adapter env passthrough = %v, want [CLAUDE_CONFIG_DIR]", gotEnvPassthrough)
	}

	gateOnly := []apiv1.Workflow{{Spec: apiv1.WorkflowSpec{Gates: []apiv1.Gate{{
		Name: "review", Evaluator: apiv1.EvaluatorAgentic,
		Agentic: &apiv1.AgenticGate{Goober: "reviewer"},
	}}}}}
	info, _, err = preflightAgenticHarnesses(
		map[string]apiv1.GooberSpec{"reviewer": {}},
		gateOnly,
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("reviewer-only default harness preflight: %v", err)
	}
	if got := info[apiv1.HarnessCopilot].Version; got != "copilot version 1.2.3" {
		t.Fatalf("reviewer preflight version = %q", got)
	}
}

// TestPreflightAgenticHarnessesStructuralFailureStillFatal proves #2812
// didn't blunt every failure mode: harnessAdapterFor itself erroring means
// the harness isn't wired up at all (unknown type, broken adapter
// construction) — a config/code defect, not a live-probe hiccup — and that
// still fails the whole call, unlike a live preflight probe failure.
func TestPreflightAgenticHarnessesStructuralFailureStillFatal(t *testing.T) {
	orig := harnessAdapterFor
	t.Cleanup(func() { harnessAdapterFor = orig })

	goobers := map[string]apiv1.GooberSpec{"nominator": {Harness: apiv1.HarnessCopilot}}
	agentic := []apiv1.Workflow{{Spec: apiv1.WorkflowSpec{Tasks: []apiv1.Task{
		{Name: "nominate", Type: apiv1.TaskAgentic, Goober: "nominator"},
	}}}}

	harnessAdapterFor = func(apiv1.Harness, []string, map[string][]string) (harness.Adapter, error) {
		return nil, errors.New("boom: harness not wired up")
	}
	if _, _, err := preflightAgenticHarnesses(goobers, agentic, nil, nil); err == nil {
		t.Fatal("expected a structural adapter-construction failure to fail the whole preflight call")
	}
}

// TestPreflightAgenticHarnessesPartialAvailability is the #2812 scenario
// that motivated this change: two workflows on two different harnesses,
// one broken and one healthy. The broken harness must not prevent the
// healthy one's workflow from preflighting successfully.
func TestPreflightAgenticHarnessesPartialAvailability(t *testing.T) {
	orig := harnessAdapterFor
	t.Cleanup(func() { harnessAdapterFor = orig })

	goobers := map[string]apiv1.GooberSpec{
		"curator":     {Harness: apiv1.HarnessCopilot},
		"implementer": {Harness: apiv1.Harness("claude-code")},
	}
	workflows := []apiv1.Workflow{
		{Spec: apiv1.WorkflowSpec{Tasks: []apiv1.Task{
			{Name: "curate", Type: apiv1.TaskAgentic, Goober: "curator"},
		}}},
		{Spec: apiv1.WorkflowSpec{Tasks: []apiv1.Task{
			{Name: "implement", Type: apiv1.TaskAgentic, Goober: "implementer"},
		}}},
	}

	harnessAdapterFor = func(h apiv1.Harness, _ []string, _ map[string][]string) (harness.Adapter, error) {
		if h == apiv1.HarnessCopilot {
			return &harness.CopilotAdapter{Command: []string{"echo"}, Runner: &harnessFakeRunner{exit: 1}}, nil
		}
		return &harness.CopilotAdapter{Command: []string{"echo"}, Runner: &harnessFakeRunner{exit: 0}}, nil
	}

	info, failures, err := preflightAgenticHarnesses(goobers, workflows, nil, nil)
	if err != nil {
		t.Fatalf("a broken harness must not fail preflight for a workflow on a healthy one: %v", err)
	}
	if failures[apiv1.HarnessCopilot] == nil {
		t.Fatal("expected copilot to be recorded as failed")
	}
	if _, ok := info[apiv1.Harness("claude-code")]; !ok {
		t.Fatal("expected claude-code (the healthy harness) to preflight successfully despite copilot failing")
	}
}

// TestAdapterForConfiguresAuthProbe proves the #238 wiring: the default
// CopilotAdapter carries the auth probe (copilotAuthCheckArgs), so every
// preflight through adapterFor — validate --check-harness AND the automatic
// daemon-startup preflight — verifies sign-in, not just CLI presence.
func TestAdapterForConfiguresAuthProbe(t *testing.T) {
	a, err := adapterFor(apiv1.HarnessCopilot, []string{"CLAUDE_CONFIG_DIR"}, nil)
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
	if strings.Join(ca.ExtraEnvAllowlist, ",") != "CLAUDE_CONFIG_DIR" {
		t.Fatalf("ExtraEnvAllowlist = %v, want [CLAUDE_CONFIG_DIR]", ca.ExtraEnvAllowlist)
	}
}

// TestPreflightAgenticHarnessesCatchesSignedOut is #238 AC3, updated for
// #2812: a harness that is installed (--version exits 0) but signed out (the
// auth probe exits non-zero) is recorded as a failure by the automatic
// daemon-startup preflight — the #284 incident is still caught at startup
// (surfaced as a warning, not buried in a burned mid-run agentic attempt),
// it just no longer blocks every other harness's workflows too.
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
	harnessAdapterFor = func(apiv1.Harness, []string, map[string][]string) (harness.Adapter, error) {
		return &harness.CopilotAdapter{
			Command:       []string{"echo"},
			AuthCheckArgs: copilotAuthCheckArgs,
			Runner:        &authProbeFakeRunner{versionExit: 0, authExit: 1},
		}, nil
	}
	_, failures, err := preflightAgenticHarnesses(goobers, agentic, nil, nil)
	if err != nil {
		t.Fatalf("a signed-out harness must not fail the whole preflight call: %v", err)
	}
	failure := failures[apiv1.HarnessCopilot]
	if failure == nil {
		t.Fatal("expected the signed-out harness to be recorded as failed")
	}
	if !strings.Contains(failure.Error(), "sign-in check") {
		t.Fatalf("failure = %v, want it to mention the sign-in check (the auth probe, not the version check)", failure)
	}
}
