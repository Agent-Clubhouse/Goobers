package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/invoke"
	harnesstest "github.com/goobers/goobers/test/testsupport/harness"
)

// TestWorkerScopesHarnessPreflightFailureToDependentWorkflow is #5949: the
// worker's gaggle kit preflights every workflow's harness, and one workflow's
// transient harness failure used to fail the whole build — so an unrelated
// workflow's activity (observed: a merge-review automated gate) failed on a
// sibling's rate-limited Copilot sign-in check.
//
// Two workflows, two harnesses: default-implement's coder runs on copilot,
// whose preflight fails; second-implement's reviewer runs on claude-code,
// which is healthy. The unrelated workflow's activities must proceed, the
// dependent workflow's agentic stage must fail as a RETRYABLE infrastructure
// failure, and the failure must not be cached: once copilot recovers, the next
// activity rebuilds and serves it.
func TestWorkerScopesHarnessPreflightFailureToDependentWorkflow(t *testing.T) {
	root := initDemo(t)
	addClaudeCodeSiblingWorkflow(t, root)

	previous := preflightHarnesses
	preflightHarnesses = preflightAgenticHarnesses
	t.Cleanup(func() { preflightHarnesses = previous })
	var copilotDown atomic.Bool
	copilotDown.Store(true)
	rateLimited := errors.New("sign-in check exited 1: Failed to fetch PAT user login (403): API rate limit exceeded")
	withHarnessAdapter(t, func(h apiv1.Harness, _ harness.EnvironmentConfig, _ map[string][]string, _ func(context.Context) (string, error)) (harness.Adapter, error) {
		if h == apiv1.HarnessCopilot && copilotDown.Load() {
			return &harnesstest.FakeAdapter{AdapterName: string(h), PreflightErr: rateLimited}, nil
		}
		return &harnesstest.FakeAdapter{AdapterName: string(h), Version: string(h) + " 1.0.0"}, nil
	})

	seams := workerReloadSeams(t, root)

	// An unrelated activity — the automated gate the issue observed failing —
	// proceeds although a sibling workflow's harness is down.
	activity := &engine.Activities{Auto: seams.Automated()}
	inputs, err := gate.AutomatedInputs(apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Outputs: map[string]any{"verdict": "pass"}})
	if err != nil {
		t.Fatal(err)
	}
	conf := apiv1.AutomatedGate{Check: "output-equals", Params: map[string]string{"key": "verdict", "equals": "pass"}}
	autoEnv := apiv1.InvocationEnvelope{RunID: "healthy-run", Gaggle: pinGaggle, WorkflowID: "second-implement", TaskID: "healthy-run:verdict", Inputs: inputs}
	if outcome, err := activity.EvaluateAutomated(context.Background(), conf, autoEnv); err != nil || outcome != gate.OutcomePass {
		t.Fatalf("unrelated automated gate failed on a sibling's harness: outcome=%q err=%v", outcome, err)
	}

	// The dependent workflow's agentic stage fails retryably, naming the cause.
	dependent := apiv1.InvocationEnvelope{RunID: "dependent-run", Gaggle: pinGaggle, WorkflowID: pinWorkflow, TaskID: "dependent-run:implement", Goober: pinGoober}
	_, err = seams.Agentic().Invoke(context.Background(), dependent)
	if err == nil || !invoke.IsInfrastructureFailure(err) || !strings.Contains(err.Error(), "API rate limit exceeded") {
		t.Fatalf("dependent agentic stage = %v, want a retryable infrastructure failure naming the harness cause", err)
	}
	if _, err := seams.Agentic().Review(context.Background(), dependent); err == nil || !invoke.IsInfrastructureFailure(err) {
		t.Fatalf("dependent agentic review = %v, want a retryable infrastructure failure", err)
	}
	// An envelope with no workflow name cannot be scoped, so it stays refused.
	if _, err := seams.Agentic().Invoke(context.Background(), apiv1.InvocationEnvelope{RunID: "unscoped", Gaggle: pinGaggle, Goober: pinGoober}); err == nil || !invoke.IsInfrastructureFailure(err) {
		t.Fatalf("unscoped agentic stage = %v, want a retryable infrastructure failure", err)
	}

	// The healthy workflow's agentic stage passes the harness check: it gets
	// as far as goober selection (a missing goober keeps the test from
	// launching a real harness binary).
	healthy := apiv1.InvocationEnvelope{RunID: "healthy-run", Gaggle: pinGaggle, WorkflowID: "second-implement", TaskID: "healthy-run:implement", Goober: "missing-goober"}
	if _, err := seams.Agentic().Invoke(context.Background(), healthy); err == nil || invoke.IsInfrastructureFailure(err) || !strings.Contains(err.Error(), `goober "missing-goober" not found in config`) {
		t.Fatalf("healthy workflow's agentic stage was not scoped past the harness failure: %v", err)
	}

	// A degraded kit is never cached, so the failure is not permanent.
	if snapshot := seams.snapshot.Load(); snapshot == nil || snapshot.gaggles[pinGaggle] != nil {
		t.Fatal("a kit with a failed harness preflight was cached")
	}
	copilotDown.Store(false)
	_, err = seams.Agentic().Invoke(context.Background(), apiv1.InvocationEnvelope{RunID: "dependent-run", Gaggle: pinGaggle, WorkflowID: pinWorkflow, TaskID: "dependent-run:implement", Goober: "missing-goober"})
	if err == nil || invoke.IsInfrastructureFailure(err) || !strings.Contains(err.Error(), `goober "missing-goober" not found in config`) {
		t.Fatalf("recovered harness still refused: %v", err)
	}
	if snapshot := seams.snapshot.Load(); snapshot == nil || snapshot.gaggles[pinGaggle] == nil || snapshot.gaggles[pinGaggle].seams.degraded() {
		t.Fatal("a healthy kit was not cached after the harness recovered")
	}
}

// addClaudeCodeSiblingWorkflow adds a second workflow to the demo gaggle whose
// agentic stage runs a second goober on the claude-code harness.
func addClaudeCodeSiblingWorkflow(t *testing.T, root string) {
	t.Helper()
	gaggleDir := filepath.Join(instance.NewLayout(root).ConfigDir(), "gaggles", pinGaggle)
	coderDir := filepath.Join(gaggleDir, "goobers", pinGoober)
	reviewerDir := filepath.Join(gaggleDir, "goobers", "reviewer")
	if err := os.MkdirAll(reviewerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	goober := readFileContent(t, filepath.Join(coderDir, "goober.yaml"))
	for old, replacement := range map[string]string{
		"  name: coder\n":           "  name: reviewer\n",
		"  harness: copilot\n":      "  harness: claude-code\n",
		"    - default-implement\n": "    - second-implement\n",
		"  displayName: Coder\n":    "  displayName: Reviewer\n",
		"  model: auto\n":           "  model: sonnet\n",
		"  harnessOptions: {}\n":    "",
	} {
		if !strings.Contains(goober, old) {
			t.Fatalf("demo goober fixture drifted: %q missing", old)
		}
		goober = strings.Replace(goober, old, replacement, 1)
	}
	writeFileContent(t, filepath.Join(reviewerDir, "goober.yaml"), goober)
	writeFileContent(t, filepath.Join(reviewerDir, "instructions.md"), readFileContent(t, filepath.Join(coderDir, "instructions.md")))

	flow := readFileContent(t, filepath.Join(gaggleDir, "workflows", pinWorkflow+".yaml"))
	for old, replacement := range map[string]string{
		"  name: default-implement\n": "  name: second-implement\n",
		"      goober: coder\n":       "      goober: reviewer\n",
	} {
		if !strings.Contains(flow, old) {
			t.Fatalf("demo workflow fixture drifted: %q missing", old)
		}
		flow = strings.Replace(flow, old, replacement, 1)
	}
	writeFileContent(t, filepath.Join(gaggleDir, "workflows", "second-implement.yaml"), flow)
}
