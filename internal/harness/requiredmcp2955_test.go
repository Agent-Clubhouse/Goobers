package harness

import (
	"context"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// TestRefuseWhenRequiredMCPUnavailable is #2955's core contract.
//
// Goobers wires the credential-free, workspace-scoped goobers-io server into
// eligible Copilot stages and instructs the model to use its artifact and
// input tools instead of ordinary file tools. When the CLI's third-party MCP
// policy is disabled it skips the server:
//
//	Skipping third-party MCP server "goobers-io" because the MCP third-party
//	policy is not enabled
//
// and the invocation carried on. The stage contract could no longer be
// satisfied, nothing said so, and the run spent an agentic attempt and
// workflow budget on an answer nothing should trust.
func TestRefuseWhenRequiredMCPUnavailable(t *testing.T) {
	tests := []struct {
		name          string
		failures      []MCPServerFailure
		reported      apiv1.ResultStatus
		wantStatus    apiv1.ResultStatus
		wantCode      string
		wantRetryable bool
	}{
		{
			name:       "policy rejection overrides a reported success",
			failures:   []MCPServerFailure{{Server: goobersIOServerName, Status: copilotMCPStatusPolicyRejected}},
			reported:   apiv1.ResultSuccess,
			wantStatus: apiv1.ResultFailure,
			wantCode:   ErrorCodeRequiredMCPRejected,
			// The policy refuses the server on every attempt, so a repass
			// spends budget to reach the same place.
			wantRetryable: false,
		},
		{
			name:       "launch failure is a different, retryable code",
			failures:   []MCPServerFailure{{Server: goobersIOServerName, Status: copilotMCPStatusAbsent}},
			reported:   apiv1.ResultSuccess,
			wantStatus: apiv1.ResultFailure,
			wantCode:   ErrorCodeRequiredMCPUnavailable,
			// A server that failed to launch may well come up next attempt.
			wantRetryable: true,
		},
		{
			name:          "handshake failure is also the availability code",
			failures:      []MCPServerFailure{{Server: goobersIOServerName, Status: copilotMCPStatusHandshakeIncomplete}},
			reported:      apiv1.ResultSuccess,
			wantStatus:    apiv1.ResultFailure,
			wantCode:      ErrorCodeRequiredMCPUnavailable,
			wantRetryable: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := apiv1.ResultEnvelope{Status: tt.reported, Summary: "did the work"}
			refuseWhenRequiredMCPUnavailable(&result, tt.failures)

			if result.Status != tt.wantStatus {
				t.Fatalf("status = %q, want %q: a model cannot report success for work it was told to do "+
					"through tools it did not have", result.Status, tt.wantStatus)
			}
			if result.Error == nil || result.Error.Code != tt.wantCode {
				t.Fatalf("error = %+v, want code %q", result.Error, tt.wantCode)
			}
			if result.Error.Retryable != tt.wantRetryable {
				t.Fatalf("retryable = %v, want %v", result.Error.Retryable, tt.wantRetryable)
			}
			if got := result.Outputs["requiredMCPUnavailable"]; got != goobersIOServerName {
				t.Fatalf("outputs[requiredMCPUnavailable] = %v, want %q", got, goobersIOServerName)
			}
		})
	}
}

// TestRequiredMCPDiagnosticNamesTheOperatorAction is the acceptance criterion
// that the diagnostic distinguish a policy rejection from a startup failure.
// The two messages must send an operator to different places: an administrator
// setting versus a fault to investigate.
func TestRequiredMCPDiagnosticNamesTheOperatorAction(t *testing.T) {
	policy := apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}
	refuseWhenRequiredMCPUnavailable(&policy,
		[]MCPServerFailure{{Server: goobersIOServerName, Status: copilotMCPStatusPolicyRejected}})

	startup := apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}
	refuseWhenRequiredMCPUnavailable(&startup,
		[]MCPServerFailure{{Server: goobersIOServerName, Status: copilotMCPStatusAbsent}})

	if !strings.Contains(policy.Error.Message, "third-party MCP policy") {
		t.Fatalf("policy message = %q, want it to name the policy to enable", policy.Error.Message)
	}
	// The documented policy-compatible path: the claude-code adapter registers
	// goobers-io without needing the third-party policy.
	if !strings.Contains(policy.Error.Message, "claude-code") {
		t.Fatalf("policy message = %q, want it to name a policy-compatible path", policy.Error.Message)
	}
	if strings.Contains(startup.Error.Message, "policy") &&
		!strings.Contains(startup.Error.Message, "rather than a policy decision") {
		t.Fatalf("startup message = %q, want it NOT to read as a policy problem", startup.Error.Message)
	}
	if policy.Error.Code == startup.Error.Code {
		t.Fatal("policy rejection and startup failure share an error code; the acceptance criteria " +
			"require them to be distinguishable")
	}
}

// TestRequiredMCPLeavesOtherServersToTheAnnotation keeps #3356's additive
// contract where it still holds. A goober may declare an MCP server its stage
// can do without, and the loss of one is journalled, not fatal. goobers-io is
// the exception because the harness registers it and the prompt depends on it.
func TestRequiredMCPLeavesOtherServersToTheAnnotation(t *testing.T) {
	result := apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "did the work"}
	refuseWhenRequiredMCPUnavailable(&result, []MCPServerFailure{
		{Server: "acme-tools", Status: copilotMCPStatusAbsent},
	})

	if result.Status != apiv1.ResultSuccess {
		t.Fatalf("status = %q, want success: a declared, optional server's loss stays an annotation",
			result.Status)
	}
	if result.Error != nil {
		t.Fatalf("error = %+v, want none", result.Error)
	}
}

// TestCopilotMCPPolicyRejectionParsing pins the log line the CLI actually
// writes, and the two ways this must not fire: a policy line naming no server,
// and an unrelated line. A false positive here fails a stage that ran fine.
func TestCopilotMCPPolicyRejectionParsing(t *testing.T) {
	tests := []struct {
		name string
		line string
		want string
	}{
		{
			name: "observed CLI line",
			line: `[WARN] Skipping third-party MCP server "goobers-io" because the MCP third-party policy is not enabled`,
			want: goobersIOServerName,
		},
		{
			name: "another server",
			line: `Skipping third-party MCP server "acme-tools" because the MCP third-party policy is not enabled`,
			want: "acme-tools",
		},
		{name: "policy phrase with no server named", line: `the MCP third-party policy is not enabled`},
		{name: "unrelated line", line: `[INFO] [rust:rmcp::service] Service initialized as client`},
		{name: "empty", line: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := copilotMCPPolicyRejectedServerName(tt.line)
			if tt.want == "" {
				if ok {
					t.Fatalf("parsed %q as a rejection of %q, want no match", tt.line, got)
				}
				return
			}
			if !ok || got != tt.want {
				t.Fatalf("server = %q, ok = %v, want %q", got, ok, tt.want)
			}
		})
	}
}

// TestExecutorFailsWhenRequiredGoobersIOWasRejected covers the call site
// rather than the helper: the unit tests above pass with the executor's call
// deleted, so this is what proves the refusal is actually wired into a run.
//
// It is also the counterpart to TestExecutorJournalsMCPServerUnavailable,
// which pins #3356's additive annotation for a goober-declared server. The two
// together state the whole contract: an optional server's loss is journalled,
// and the required one's loss fails the stage.
func TestExecutorFailsWhenRequiredGoobersIOWasRejected(t *testing.T) {
	rec := &fakeRecorder{}
	adapter := &mcpFailureFakeAdapter{
		FakeAdapter: FakeAdapter{Act: func(_ context.Context, req RunRequest) error {
			// The agent reports success: it had no way to know its artifact
			// tools were missing, which is exactly why its status cannot be
			// the evidence here.
			return WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{
				Status: apiv1.ResultSuccess, Summary: "implemented the change",
			})
		}},
		failures: []MCPServerFailure{{
			Server: goobersIOServerName, Status: copilotMCPStatusPolicyRejected,
		}},
	}
	exec, err := NewExecutor(
		adapter,
		testInjector(t, "", "", noopRegistrar{}),
		rec, rec, rec,
		journal.NewPatternScrubber(),
		"",
	)
	if err != nil {
		t.Fatal(err)
	}

	result, err := exec.Invoke(context.Background(), testEnvelope(t.TempDir()))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result.Status != apiv1.ResultFailure {
		t.Fatalf("status = %q, want failure: the stage ran without the artifact I/O tools its own "+
			"prompt told it to use", result.Status)
	}
	if result.Error == nil || result.Error.Code != ErrorCodeRequiredMCPRejected {
		t.Fatalf("error = %+v, want code %q", result.Error, ErrorCodeRequiredMCPRejected)
	}
}
