package harness

import (
	"context"
	"slices"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// surfaceFakeAdapter is a FakeAdapter that reports a fixed tool surface, so a
// test can force a granted surface that does not match the declared groups —
// #2184's condition, where the "github" group was declared and expanded to
// read-only tools.
type surfaceFakeAdapter struct {
	FakeAdapter
	surface []string
	ran     bool
}

func (a *surfaceFakeAdapter) AvailableTools([]string) []string { return a.surface }

func (a *surfaceFakeAdapter) Run(ctx context.Context, req RunRequest) (Outcome, error) {
	a.ran = true
	return a.FakeAdapter.Run(ctx, req)
}

func capabilityExecutor(t *testing.T, adapter Adapter, tools []string) *Executor {
	t.Helper()
	rec := &fakeRecorder{}
	exec, err := NewExecutor(
		adapter,
		testInjector(t, "", "", noopRegistrar{}),
		rec, rec, rec,
		journal.NewPatternScrubber(),
		"",
		WithTools(tools),
	)
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	return exec
}

func writeSuccess(_ context.Context, req RunRequest) error {
	return WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
}

// TestExecutorPreflightFailsWriteIncapableGitHubSurface is #2197's core
// acceptance for the preflight half: a goober declaring the github tool group
// alongside github:issues:write, whose granted surface expands to read-only
// github tools, fails the stage with ResultFailure before the model runs — not
// ResultBlocked, which would needs-human-park the run's whole claimed batch.
func TestExecutorPreflightFailsWriteIncapableGitHubSurface(t *testing.T) {
	adapter := &surfaceFakeAdapter{
		FakeAdapter: FakeAdapter{Act: writeSuccess},
		surface: []string{
			"github-mcp-server-issue_read",
			"github-mcp-server-list_issues",
			"github-mcp-server-search_issues",
		},
	}
	exec := capabilityExecutor(t, adapter, []string{"github", "shell"})

	result, err := exec.Invoke(context.Background(), testEnvelope(t.TempDir(), "github:issues:write", "agent:model"))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result.Status != apiv1.ResultFailure {
		t.Fatalf("status = %q, want failure (blocked parks the whole claimed batch)", result.Status)
	}
	if result.Error == nil || result.Error.Code != ErrorCodeCapabilityUnsatisfied {
		t.Fatalf("error = %+v, want code %q", result.Error, ErrorCodeCapabilityUnsatisfied)
	}
	if result.Error.Retryable {
		t.Fatal("capability preflight failure must be non-retryable")
	}
	if !strings.Contains(result.Error.Message, "github:issues:write") {
		t.Fatalf("error message = %q, want it to name the unsatisfied capability", result.Error.Message)
	}
	if adapter.ran {
		t.Fatal("preflight must fail the stage before the harness session runs")
	}
}

// TestExecutorPreflightReviewFailsVerdict proves the preflight is generic
// across agentic stages, not just invoked tasks: a reviewer gate with the same
// unsatisfiable capability fails its verdict instead of running the model.
func TestExecutorPreflightReviewFailsVerdict(t *testing.T) {
	adapter := &surfaceFakeAdapter{
		FakeAdapter: FakeAdapter{Act: writeSuccess},
		surface:     []string{"github-mcp-server-issue_read"},
	}
	exec := capabilityExecutor(t, adapter, []string{"github"})

	verdict, err := exec.Review(context.Background(), testEnvelope(t.TempDir(), "github:issues:write"))
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if verdict.Decision != apiv1.VerdictFail {
		t.Fatalf("decision = %q, want fail", verdict.Decision)
	}
	if !strings.Contains(verdict.Summary, ErrorCodeCapabilityUnsatisfied) {
		t.Fatalf("summary = %q, want it to name the capability failure", verdict.Summary)
	}
	if adapter.ran {
		t.Fatal("preflight must fail the gate before the harness session runs")
	}
}

// TestExecutorPreflightAdmitsSatisfiedSurface pins the no-regression side: a
// surface that can exercise the declared capability runs normally.
func TestExecutorPreflightAdmitsSatisfiedSurface(t *testing.T) {
	adapter := &surfaceFakeAdapter{
		FakeAdapter: FakeAdapter{Act: writeSuccess},
		surface:     copilotAvailableTools(RunRequest{Tools: []string{"github", "shell"}}),
	}
	exec := capabilityExecutor(t, adapter, []string{"github", "shell"})

	result, err := exec.Invoke(context.Background(), testEnvelope(t.TempDir(), "github:issues:write", "github:issues:read"))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result.Status != apiv1.ResultSuccess {
		t.Fatalf("status = %q, want success", result.Status)
	}
	if !adapter.ran {
		t.Fatal("a satisfied surface must run the harness session")
	}
}

// TestShippedCopilotGitHubGroupSatisfiesWriteCapabilities guards the shipped
// allowlist itself: #2184's regression was the github group losing its write
// tools, which this preflight table would now reject at run time.
func TestShippedCopilotGitHubGroupSatisfiesWriteCapabilities(t *testing.T) {
	surface := copilotAvailableTools(RunRequest{Tools: []string{"github"}})
	for _, capability := range []string{"github:issues:write", "github:issues:read", "github:issues:approve", "github:milestones:write"} {
		if err := preflightCapabilityTools([]string{capability}, []string{"github"}, surface); err != nil {
			t.Fatalf("shipped github tool group must satisfy %s: %v", capability, err)
		}
	}
}

// TestPreflightSkipsUndeclaredGroup keeps the check scoped to declared groups:
// a goober that holds a github capability but declares only shell exercises it
// another way (the gh CLI) and must not be failed closed.
func TestPreflightSkipsUndeclaredGroup(t *testing.T) {
	surface := copilotAvailableTools(RunRequest{Tools: []string{"shell"}})
	if err := preflightCapabilityTools([]string{"github:issues:write"}, []string{"shell"}, surface); err != nil {
		t.Fatalf("preflight must not fail a capability exercised outside a declared group: %v", err)
	}
}

// TestPreflightSkippedForAdapterWithoutSurface keeps adapters that cannot
// report a surface exactly as they were.
func TestPreflightSkippedForAdapterWithoutSurface(t *testing.T) {
	exec := capabilityExecutor(t, &FakeAdapter{Act: writeSuccess}, []string{"github"})
	result, err := exec.Invoke(context.Background(), testEnvelope(t.TempDir(), "github:issues:write"))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result.Status != apiv1.ResultSuccess {
		t.Fatalf("status = %q, want success", result.Status)
	}
}

// TestReclassifyMissingCapabilityBlock is #2197's backstop half: a goober that
// self-reports blocked because it lost a tool it was told it had becomes a
// failure, so the run ends PhaseFailed (comment-only, no label) instead of
// escalating and needs-human-parking every claimed item.
func TestReclassifyMissingCapabilityBlock(t *testing.T) {
	for _, code := range []string{"MISSING_CAPABILITY", "WRITE_TOOL_UNAVAILABLE", "missing-tool", "CAPABILITY_NOT_GRANTED"} {
		result := apiv1.ResultEnvelope{
			Status:  apiv1.ResultBlocked,
			Summary: "no write tool is available in this session",
			Error:   &apiv1.ErrorInfo{Code: code, Message: "the github MCP server exposes no issue_write tool"},
		}
		reclassifyMissingCapabilityBlock(&result)
		if result.Status != apiv1.ResultFailure {
			t.Fatalf("code %q: status = %q, want failure", code, result.Status)
		}
		if result.Error.Code != ErrorCodeCapabilityUnsatisfied || result.Error.Retryable {
			t.Fatalf("code %q: error = %+v, want non-retryable %q", code, result.Error, ErrorCodeCapabilityUnsatisfied)
		}
		if !strings.Contains(result.Error.Message, "issue_write") || !strings.Contains(result.Error.Message, code) {
			t.Fatalf("code %q: message = %q, want the agent's own cause preserved", code, result.Error.Message)
		}
		if got, ok := result.Outputs["capabilityUnsatisfied"].(bool); !ok || !got {
			t.Fatalf("code %q: outputs = %+v, want capabilityUnsatisfied=true", code, result.Outputs)
		}
	}
}

// TestReclassifyMissingCapabilityLeavesGenuineBlocks pins the #544/#545
// no-regression criterion: a real per-item dependency block is untouched and
// still escalates.
func TestReclassifyMissingCapabilityLeavesGenuineBlocks(t *testing.T) {
	blocked := apiv1.ResultEnvelope{
		Status:  apiv1.ResultBlocked,
		Summary: "waiting on #441",
		Error:   &apiv1.ErrorInfo{Code: "DEPENDENCY_NOT_MET", Message: "issue 441 must merge first"},
		Outputs: map[string]interface{}{"blockedBy": "441"},
	}
	want := blocked
	reclassifyMissingCapabilityBlock(&blocked)
	if blocked.Status != want.Status || blocked.Error.Code != want.Error.Code || blocked.Summary != want.Summary {
		t.Fatalf("genuine dependency block was rewritten: %+v", blocked)
	}
	if _, ok := blocked.Outputs["capabilityUnsatisfied"]; ok {
		t.Fatalf("outputs = %+v, want the genuine block untouched", blocked.Outputs)
	}
}

// TestExecutorReclassifiesSelfReportedMissingCapability wires the backstop
// through the executor, the seam every agentic stage's result passes through.
func TestExecutorReclassifiesSelfReportedMissingCapability(t *testing.T) {
	adapter := &FakeAdapter{Act: func(_ context.Context, req RunRequest) error {
		return WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{
			Status:  apiv1.ResultBlocked,
			Summary: "I have no write tool",
			Error:   &apiv1.ErrorInfo{Code: "MISSING_CAPABILITY", Message: "github:issues:write is unavailable"},
		})
	}}
	exec := capabilityExecutor(t, adapter, []string{"shell"})

	result, err := exec.Invoke(context.Background(), testEnvelope(t.TempDir()))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result.Status != apiv1.ResultFailure {
		t.Fatalf("status = %q, want failure", result.Status)
	}
	if result.Error == nil || result.Error.Code != ErrorCodeCapabilityUnsatisfied {
		t.Fatalf("error = %+v, want code %q", result.Error, ErrorCodeCapabilityUnsatisfied)
	}
}

// TestCopilotAdapterReportsToolSurface pins the adapter seam the preflight
// depends on: the reported surface is the expansion the session receives.
func TestCopilotAdapterReportsToolSurface(t *testing.T) {
	var adapter ToolSurfaceReporter = &CopilotAdapter{}
	got := adapter.AvailableTools([]string{"github"})
	if !slices.Contains(got, "github-mcp-server-issue_write") {
		t.Fatalf("AvailableTools(github) = %v, want the expanded github group", got)
	}
}
