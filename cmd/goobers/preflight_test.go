package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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

// toolPreflightFakeRunner scripts both the base --version preflight and a
// PreflightGithubTools probe session behind one ProcessRunner, distinguishing
// them by whether --log-dir appears in the command — the tool probe's own
// signature — and counts each kind of call so tests can assert dedup.
type toolPreflightFakeRunner struct {
	registeredTools []string
	toolProbeCalls  int
}

func (r *toolPreflightFakeRunner) Run(_ context.Context, req harness.ProcessRequest) (harness.ProcessResult, error) {
	logDir := ""
	for i, arg := range req.Command {
		if arg == "--log-dir" && i+1 < len(req.Command) {
			logDir = req.Command[i+1]
		}
	}
	if logDir == "" {
		return harness.ProcessResult{ExitCode: 0, Transcript: []byte("copilot version 1.2.3\n")}, nil
	}
	r.toolProbeCalls++
	var lines []string
	for _, tool := range r.registeredTools {
		lines = append(lines, fmt.Sprintf(`{"type":"function","name": %q,"description":"..."}`, tool))
	}
	if err := os.WriteFile(filepath.Join(logDir, "process.log"), []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		return harness.ProcessResult{}, err
	}
	return harness.ProcessResult{ExitCode: 0}, nil
}

// TestPreflightAgenticHarnessesChecksGithubWriteTools is #2194's daemon-startup
// half: a task declaring github:issues:write is verified against what the
// live harness actually registers, not just what it's configured to request.
func TestPreflightAgenticHarnessesChecksGithubWriteTools(t *testing.T) {
	orig := harnessAdapterFor
	t.Cleanup(func() { harnessAdapterFor = orig })

	goobers := map[string]apiv1.GooberSpec{
		"curator": {Harness: apiv1.HarnessCopilot, Tools: []string{"github", "shell"}},
	}
	agentic := []apiv1.Workflow{{Spec: apiv1.WorkflowSpec{Tasks: []apiv1.Task{
		{Name: "curate", Type: apiv1.TaskAgentic, Goober: "curator", Capabilities: []string{"github:issues:write"}},
	}}}}

	runner := &toolPreflightFakeRunner{registeredTools: []string{
		"github-mcp-server-issue_write",
		"github-mcp-server-add_issue_comment",
		"github-mcp-server-sub_issue_write",
	}}
	harnessAdapterFor = func(apiv1.Harness) (harness.Adapter, error) {
		return &harness.CopilotAdapter{Command: []string{"echo"}, Runner: runner}, nil
	}
	if _, err := preflightAgenticHarnesses(goobers, agentic); err != nil {
		t.Fatalf("expected the write-tool preflight to pass: %v", err)
	}
	if runner.toolProbeCalls != 1 {
		t.Fatalf("tool probe calls = %d, want 1", runner.toolProbeCalls)
	}
}

// TestPreflightAgenticHarnessesFailsClosedOnMissingWriteTool reproduces
// #2184's exact shape at the daemon-startup layer: issue_write and
// sub_issue_write register but add_issue_comment silently doesn't — the
// preflight must fail closed and name it, not rely on the goober noticing.
func TestPreflightAgenticHarnessesFailsClosedOnMissingWriteTool(t *testing.T) {
	orig := harnessAdapterFor
	t.Cleanup(func() { harnessAdapterFor = orig })

	goobers := map[string]apiv1.GooberSpec{
		"curator": {Harness: apiv1.HarnessCopilot, Tools: []string{"github", "shell"}},
	}
	agentic := []apiv1.Workflow{{Spec: apiv1.WorkflowSpec{Tasks: []apiv1.Task{
		{Name: "curate", Type: apiv1.TaskAgentic, Goober: "curator", Capabilities: []string{"github:issues:write"}},
	}}}}

	runner := &toolPreflightFakeRunner{registeredTools: []string{
		"github-mcp-server-issue_write",
		"github-mcp-server-sub_issue_write",
	}}
	harnessAdapterFor = func(apiv1.Harness) (harness.Adapter, error) {
		return &harness.CopilotAdapter{Command: []string{"echo"}, Runner: runner}, nil
	}
	_, err := preflightAgenticHarnesses(goobers, agentic)
	if err == nil {
		t.Fatal("expected preflight to fail closed on a missing registered write tool")
	}
	if !strings.Contains(err.Error(), "github-mcp-server-add_issue_comment") {
		t.Fatalf("err = %v, want it to name the missing tool", err)
	}
}

// TestPreflightAgenticHarnessesSkipsWriteToolCheckWithoutCapability confirms
// the common case — a task with no write capability declared — never pays
// for the extra probe session.
func TestPreflightAgenticHarnessesSkipsWriteToolCheckWithoutCapability(t *testing.T) {
	orig := harnessAdapterFor
	t.Cleanup(func() { harnessAdapterFor = orig })

	goobers := map[string]apiv1.GooberSpec{
		"implementer": {Harness: apiv1.HarnessCopilot, Tools: []string{"shell"}},
	}
	agentic := []apiv1.Workflow{{Spec: apiv1.WorkflowSpec{Tasks: []apiv1.Task{
		{Name: "implement", Type: apiv1.TaskAgentic, Goober: "implementer"},
	}}}}

	runner := &toolPreflightFakeRunner{}
	harnessAdapterFor = func(apiv1.Harness) (harness.Adapter, error) {
		return &harness.CopilotAdapter{Command: []string{"echo"}, Runner: runner}, nil
	}
	if _, err := preflightAgenticHarnesses(goobers, agentic); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if runner.toolProbeCalls != 0 {
		t.Fatalf("tool probe calls = %d, want 0 for a task with no matching declared capability", runner.toolProbeCalls)
	}
}

// TestPreflightAgenticHarnessesDedupsWriteToolCheckBySignature confirms two
// tasks declaring an identical (harness, tools, capabilities) combination
// share one probe session rather than paying for it per task.
func TestPreflightAgenticHarnessesDedupsWriteToolCheckBySignature(t *testing.T) {
	orig := harnessAdapterFor
	t.Cleanup(func() { harnessAdapterFor = orig })

	goobers := map[string]apiv1.GooberSpec{
		"curator":   {Harness: apiv1.HarnessCopilot, Tools: []string{"github", "shell"}},
		"nominator": {Harness: apiv1.HarnessCopilot, Tools: []string{"github", "shell"}},
	}
	agentic := []apiv1.Workflow{{Spec: apiv1.WorkflowSpec{Tasks: []apiv1.Task{
		{Name: "curate", Type: apiv1.TaskAgentic, Goober: "curator", Capabilities: []string{"github:issues:write"}},
		{Name: "nominate", Type: apiv1.TaskAgentic, Goober: "nominator", Capabilities: []string{"github:issues:write"}},
	}}}}

	runner := &toolPreflightFakeRunner{registeredTools: []string{
		"github-mcp-server-issue_write",
		"github-mcp-server-add_issue_comment",
		"github-mcp-server-sub_issue_write",
	}}
	harnessAdapterFor = func(apiv1.Harness) (harness.Adapter, error) {
		return &harness.CopilotAdapter{Command: []string{"echo"}, Runner: runner}, nil
	}
	if _, err := preflightAgenticHarnesses(goobers, agentic); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if runner.toolProbeCalls != 1 {
		t.Fatalf("tool probe calls = %d, want 1 (identical harness/tools/capabilities signature should dedup)", runner.toolProbeCalls)
	}
}
