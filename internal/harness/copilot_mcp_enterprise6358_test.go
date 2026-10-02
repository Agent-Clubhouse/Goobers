package harness

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
)

// Captured Copilot CLI log shapes for #6358, with identifying detail removed.
// The lockdown refusal followed an auth-broker failure and a fail-closed
// policy state; only the refusal line names the server.
const (
	copilotLogBrokerFailure = `2026-10-01T09:14:02.118Z [ERROR] GitHub token provider callback failed: ` +
		`Entra broker could not silently acquire a token`
	copilotLogPolicyUnknown = `2026-10-01T09:14:02.120Z [WARN] [managedSettings] effective policy unknown; ` +
		`failing closed`
	copilotLogEnterpriseLockdown = `2026-10-01T09:14:02.231Z [WARN] Skipping MCP server "goobers-io": ` +
		`blocked by enterprise customization lockdown (only plugin/managed MCP servers are permitted)`
	copilotLogEnterpriseLockdownJSON = `{"level":"warn","msg":"Skipping MCP server \"goobers-io\": ` +
		`blocked by enterprise customization lockdown (only plugin/managed MCP servers are permitted)"}`
)

func TestCopilotMCPEnterpriseLockdownParsing(t *testing.T) {
	tests := []struct {
		name string
		line string
		want string
	}{
		{name: "observed CLI line", line: copilotLogEnterpriseLockdown, want: goobersIOServerName},
		{name: "JSON-escaped record", line: copilotLogEnterpriseLockdownJSON, want: goobersIOServerName},
		{
			name: "another server",
			line: `Skipping MCP server "acme-tools": blocked by enterprise customization lockdown`,
			want: "acme-tools",
		},
		{name: "phrase with no server named", line: `blocked by enterprise customization lockdown`},
		{name: "third-party policy line is not a lockdown",
			line: `Skipping third-party MCP server "goobers-io" because the MCP third-party policy is not enabled`},
		{name: "unrelated line", line: copilotLogBrokerFailure},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := copilotMCPEnterpriseBlockedServerName(tt.line)
			if tt.want == "" {
				if ok {
					t.Fatalf("parsed %q as a lockdown of %q, want no match", tt.line, got)
				}
				return
			}
			if !ok || got != tt.want {
				t.Fatalf("server = %q, ok = %v, want %q", got, ok, tt.want)
			}
		})
	}
}

func TestCopilotMCPServerFailuresNamesEnterpriseLockdown(t *testing.T) {
	dir := writeCopilotLog(t, copilotLogBrokerFailure, copilotLogPolicyUnknown, copilotLogEnterpriseLockdown)
	got := copilotMCPServerFailures(goobersIORequest(), dir)
	if len(got) != 1 || got[0].Server != goobersIOServerName || got[0].Status != copilotMCPStatusEnterpriseBlocked {
		t.Fatalf("failures = %+v, want %s/%s", got, goobersIOServerName, copilotMCPStatusEnterpriseBlocked)
	}
}

func TestRefuseWhenRequiredMCPEnterpriseBlocked(t *testing.T) {
	result := apiv1.ResultEnvelope{Status: apiv1.ResultBlocked, Summary: "input transport unavailable"}
	refuseWhenRequiredMCPUnavailable(&result,
		[]MCPServerFailure{{Server: goobersIOServerName, Status: copilotMCPStatusEnterpriseBlocked}})

	if result.Status != apiv1.ResultFailure {
		t.Fatalf("status = %q, want failure", result.Status)
	}
	if result.Error == nil || result.Error.Code != ErrorCodeRequiredMCPEnterpriseBlocked {
		t.Fatalf("error = %+v, want code %q", result.Error, ErrorCodeRequiredMCPEnterpriseBlocked)
	}
	if result.Error.Retryable {
		t.Fatal("retryable = true, want false: the lockdown refuses the server on every attempt")
	}
	for _, want := range []string{"blocked by enterprise policy", "unsupported", "goobers-io-mcp.md"} {
		if !strings.Contains(result.Error.Message, want) {
			t.Fatalf("message = %q, want it to contain %q", result.Error.Message, want)
		}
	}
	if strings.Contains(result.Error.Message, "third-party MCP policy") {
		t.Fatalf("message = %q names the third-party policy, which cannot lift a lockdown", result.Error.Message)
	}
}

func TestClassifyCopilotEnterpriseBlockFromProbeFailure(t *testing.T) {
	lockdown := writeCopilotLog(t, copilotLogBrokerFailure, copilotLogEnterpriseLockdown)
	clean := writeCopilotLog(t, copilotLogBrokerFailure)
	probeErr := fmt.Errorf("%w: %s (%s)", errRequiredMCPRejected, goobersIOServerName, "tool_authorization_failure")
	absentErr := fmt.Errorf("%w: %s (%s)", errRequiredMCPUnavailable, goobersIOServerName, "required_tool_unavailable")

	for _, runErr := range []error{probeErr, absentErr} {
		got := classifyCopilotEnterpriseBlock(runErr, lockdown)
		if !errors.Is(got, errRequiredMCPEnterpriseBlocked) || !errors.Is(got, runErr) {
			t.Fatalf("classified = %v, want enterprise block wrapping %v", got, runErr)
		}
		staged := classifyHarnessRunError(got, got)
		if invoke.IsInfrastructureFailure(staged) {
			t.Fatalf("staged = %v, want a non-infrastructure stage failure", staged)
		}
		var coded interface{ StageErrorCode() string }
		if !errors.As(staged, &coded) || coded.StageErrorCode() != ErrorCodeRequiredMCPEnterpriseBlocked {
			t.Fatalf("staged = %v, want stage error code %q", staged, ErrorCodeRequiredMCPEnterpriseBlocked)
		}
	}
	if got := classifyCopilotEnterpriseBlock(probeErr, clean); errors.Is(got, errRequiredMCPEnterpriseBlocked) {
		t.Fatalf("classified = %v, want unchanged without the lockdown line", got)
	}
	if got := classifyCopilotEnterpriseBlock(probeErr, ""); errors.Is(got, errRequiredMCPEnterpriseBlocked) {
		t.Fatalf("classified = %v, want unchanged without a log", got)
	}
	other := errors.New("process exited 1")
	if got := classifyCopilotEnterpriseBlock(other, lockdown); errors.Is(got, errRequiredMCPEnterpriseBlocked) {
		t.Fatalf("classified = %v, want an unrelated error unchanged", got)
	}
	if got := classifyCopilotEnterpriseBlock(nil, lockdown); got != nil {
		t.Fatalf("classified = %v, want nil", got)
	}
}

func TestControlledMCPPostTurnNamesEnterpriseLockdown(t *testing.T) {
	session := &readinessSession{status: "absent"}
	runner := &copilotControlledRunner{session: session}
	req := RunRequest{GoobersIORegistered: true}
	dir := writeCopilotLog(t, copilotLogEnterpriseLockdown)
	failures := copilotRunnerMCPFailures(context.Background(), runner, req, dir)
	if len(failures) != 1 || failures[0].Status != copilotMCPStatusEnterpriseBlocked {
		t.Fatalf("failures = %+v, want %s", failures, copilotMCPStatusEnterpriseBlocked)
	}
	session.status = "connected"
	if failures := copilotRunnerMCPFailures(context.Background(), runner, req, dir); len(failures) != 0 {
		t.Fatalf("failures = %+v, want none for a connected server whatever the log says", failures)
	}
}

func TestExecutorFailsWhenRequiredGoobersIOWasEnterpriseBlocked(t *testing.T) {
	rec := &fakeRecorder{}
	adapter := &mcpFailureFakeAdapter{
		FakeAdapter: FakeAdapter{Act: func(_ context.Context, req RunRequest) error {
			return WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{
				Status: apiv1.ResultBlocked, Summary: "input transport unavailable",
			})
		}},
		failures: []MCPServerFailure{{Server: goobersIOServerName, Status: copilotMCPStatusEnterpriseBlocked}},
	}
	exec, err := NewExecutor(adapter, testInjector(t, "", "", noopRegistrar{}), rec, rec, rec,
		journal.NewPatternScrubber(), "")
	if err != nil {
		t.Fatal(err)
	}
	result, err := exec.Invoke(context.Background(), testEnvelope(t.TempDir()))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result.Status != apiv1.ResultFailure || result.Error == nil ||
		result.Error.Code != ErrorCodeRequiredMCPEnterpriseBlocked {
		t.Fatalf("result = %+v, want failure with code %q", result, ErrorCodeRequiredMCPEnterpriseBlocked)
	}
}
