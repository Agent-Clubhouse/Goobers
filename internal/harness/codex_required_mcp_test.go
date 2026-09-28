package harness

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
)

// codexRequiredStartupStderr is the shape codex-cli 0.157.0 writes when a
// server registered with `required = true` fails to initialize: it exits 1
// before any model turn and writes nothing to its JSONL stdout.
const codexRequiredStartupStderr = "2026-01-01T00:00:00.000000Z ERROR codex_core::session: Failed to create session: " +
	"required MCP servers failed to initialize: goobers-io: handshaking with MCP server failed: secret-server-detail\n" +
	"Error: thread/start: thread/start failed: error creating thread: Fatal error: Failed to initialize session: " +
	"required MCP servers failed to initialize: goobers-io: handshaking with MCP server failed: secret-server-detail (code -32603)\n"

func runCodexForRequiredMCP(t *testing.T, runner ProcessRunner) ([]MCPReadiness, error) {
	t.Helper()
	workspace := t.TempDir()
	var reports []MCPReadiness
	adapter := &CodexAdapter{
		Command:         []string{"codex"},
		Runner:          runner,
		SelfBin:         "/usr/local/bin/goobers",
		EnvCapabilities: map[string]string{"agent:model": codexModelEnv},
	}
	_, err := adapter.Run(context.Background(), RunRequest{
		Envelope:       testEnvelope(workspace, "agent:model"),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Credentials:    pushCredentials(t, "agent:model", "sk-test-codex"),
		MCPReadinessSink: func(report MCPReadiness) error {
			reports = append(reports, report)
			return nil
		},
	})
	return reports, err
}

// TestCodexRequiredGoobersIOStartupFailureIsInfrastructure is the codex side
// of #5397: goobers-io is registered with `required = true`, so the Codex CLI
// refuses to start without it and exits before the model runs. That exit must
// carry the required-MCP infrastructure classification (so stage and gate
// infrastructure retries apply) instead of arriving as a generic harness
// error, and it must record a readiness observation.
func TestCodexRequiredGoobersIOStartupFailureIsInfrastructure(t *testing.T) {
	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 1, Stderr: []byte(codexRequiredStartupStderr), Transcript: []byte(codexRequiredStartupStderr)},
		err:    errors.New("harness: run [codex exec]: exit status 1"),
	}
	reports, err := runCodexForRequiredMCP(t, runner)
	if !errors.Is(err, errRequiredMCPUnavailable) {
		t.Fatalf("Run error = %v, want errRequiredMCPUnavailable", err)
	}
	if strings.Contains(err.Error(), "secret-server-detail") {
		t.Fatalf("error copied the CLI's server detail: %v", err)
	}
	classified := classifyHarnessRunError(err, err)
	if !invoke.IsInfrastructureFailure(classified) {
		t.Fatalf("classified error = %v, want infrastructure failure", classified)
	}
	var coded interface{ StageErrorCode() string }
	if !errors.As(classified, &coded) || coded.StageErrorCode() != ErrorCodeRequiredMCPUnavailable {
		t.Fatalf("classified error = %v, want stage error code %q", classified, ErrorCodeRequiredMCPUnavailable)
	}
	want := []MCPReadiness{
		{Server: goobersIOServerName, Category: "check_unobservable", Source: codexMCPReadinessSource, Connection: "unobservable", Inventory: "unobservable", Authorization: "unobservable", ObservedStatus: mcpObservedUnobserved},
		{Server: goobersIOServerName, Category: "transport_failure", Source: codexMCPReadinessSource, Connection: "unobservable", Inventory: "unobservable", Authorization: "unobservable", ObservedStatus: mcpObservedUnobserved},
	}
	if !slices.Equal(reports, want) {
		t.Fatalf("readiness reports = %+v, want %+v", reports, want)
	}
}

// TestCodexOtherStartupFailuresKeepTheirClassification confirms only a
// goobers-io required-startup failure is reclassified: a declared server's
// required-startup failure (usually configuration) and an unrelated process
// failure keep the ordinary harness error.
func TestCodexOtherStartupFailuresKeepTheirClassification(t *testing.T) {
	for name, stderr := range map[string]string{
		"declared server":   "Error: Failed to initialize session: required MCP servers failed to initialize: context: connection closed\n",
		"unrelated failure": "Error: unexpected argument\n",
	} {
		t.Run(name, func(t *testing.T) {
			runner := &fakeProcessRunner{
				result: ProcessResult{ExitCode: 1, Stderr: []byte(stderr)},
				err:    errors.New("harness: run [codex exec]: exit status 1"),
			}
			reports, err := runCodexForRequiredMCP(t, runner)
			if err == nil || errors.Is(err, errRequiredMCPUnavailable) {
				t.Fatalf("Run error = %v, want an unclassified failure", err)
			}
			if len(reports) != 1 || reports[0].Category != "check_unobservable" {
				t.Fatalf("readiness reports = %+v, want only the pre-model check_unobservable", reports)
			}
		})
	}
}

// TestCodexReportsRequiredMCPReadinessForStartedSession confirms a successful codex run
// records the check_unobservable observation naming the CLI's own required
// startup enforcement, then availability evidence once the session started.
func TestCodexReportsRequiredMCPReadinessForStartedSession(t *testing.T) {
	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0, Transcript: []byte(codexCompletedStream)},
		act: func(req ProcessRequest) error {
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	reports, err := runCodexForRequiredMCP(t, runner)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !slices.Equal(reports, codexStartedSessionReports) {
		t.Fatalf("readiness reports = %+v, want %+v", reports, codexStartedSessionReports)
	}
}

// TestCodexReportsNothingWithoutGoobersIO confirms an unregistered goobers-io
// records no readiness observation at all.
func TestCodexReportsNothingWithoutGoobersIO(t *testing.T) {
	workspace := t.TempDir()
	var reports []MCPReadiness
	adapter := &CodexAdapter{
		Command:         []string{"codex"},
		Runner:          &codexStartupRunner{steps: []codexStartupStep{{stdout: codexCompletedStream}}},
		EnvCapabilities: map[string]string{"agent:model": codexModelEnv},
	}
	_, err := adapter.Run(context.Background(), RunRequest{
		Envelope:       testEnvelope(workspace, "agent:model"),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Credentials:    pushCredentials(t, "agent:model", "sk-test-codex"),
		MCPReadinessSink: func(report MCPReadiness) error {
			reports = append(reports, report)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var want []MCPReadiness
	if !slices.Equal(reports, want) {
		t.Fatalf("readiness reports = %+v, want %+v", reports, want)
	}
}

func TestCodexRequiredMCPStartupFailed(t *testing.T) {
	for _, tc := range []struct {
		output string
		want   bool
	}{
		{output: codexRequiredStartupStderr, want: true},
		{output: "x: required MCP servers failed to initialize: context: boom; goobers-io: closed\n", want: true},
		{output: "x: required MCP servers failed to initialize: context: mentions goobers-io: inside\n", want: false},
		{output: "MCP client for `goobers-io` failed to start\n", want: false},
		{output: "", want: false},
	} {
		if got := codexRequiredMCPStartupFailed([]byte(tc.output), goobersIOServerName); got != tc.want {
			t.Errorf("codexRequiredMCPStartupFailed(%q) = %v, want %v", tc.output, got, tc.want)
		}
	}
}

// codexStartedSessionReports is what a codex stage whose session started
// records: the check_unobservable made before the CLI runs, then the
// availability evidence the started session supplies.
var codexStartedSessionReports = []MCPReadiness{
	{Server: goobersIOServerName, Category: "check_unobservable", Source: codexMCPReadinessSource, Connection: "unobservable", Inventory: "unobservable", Authorization: "unobservable", ObservedStatus: mcpObservedUnobserved},
	{Server: goobersIOServerName, Category: "check_unobservable", Source: codexMCPReadinessSource, Connection: "ready", Inventory: "ready", Authorization: "unobservable", ObservedStatus: mcpObservedUnobserved},
}

// codexStartupStep is one scripted codex invocation: stdout feeds the JSONL
// capture, the transcript carries both streams, and a completion is written
// after the last step that succeeds.
type codexStartupStep struct {
	stdout string
	stderr string
	err    error
}

type codexStartupRunner struct {
	steps []codexStartupStep
	calls int
}

func (r *codexStartupRunner) Run(_ context.Context, req ProcessRequest) (ProcessResult, error) {
	step := r.steps[r.calls]
	r.calls++
	if req.StdoutCapture != nil && step.stdout != "" {
		_, _ = req.StdoutCapture.Write([]byte(step.stdout))
	}
	exit := 0
	if step.err != nil {
		exit = 1
	}
	result := ProcessResult{ExitCode: exit, Transcript: []byte(step.stdout + step.stderr), Stderr: []byte(step.stderr)}
	if step.err == nil && r.calls == len(r.steps) {
		if err := WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}); err != nil {
			return result, err
		}
	}
	return result, step.err
}

// codexToolOutputQuotingMarker is a codex turn that started, ran a command
// whose output quotes the required-startup marker (a grep over this very
// repository would), and then failed.
const codexToolOutputQuotingMarker = `{"type":"thread.started","thread_id":"thread-quote"}
{"type":"turn.started"}
{"type":"item.completed","item":{"type":"command_execution","aggregated_output":"required MCP servers failed to initialize: goobers-io: handshaking with MCP server failed: quoted\n"}}
{"type":"turn.failed","error":{"message":"model error"}}
`

// TestCodexStartedSessionFailureIsNotRequiredMCPStartup confirms a codex turn
// that started and then failed is never classified as a pre-model
// required-MCP startup failure, even when its output or stderr quotes the
// CLI's refusal text, and records availability evidence rather than a
// transport_failure.
func TestCodexStartedSessionFailureIsNotRequiredMCPStartup(t *testing.T) {
	for name, stderr := range map[string]string{
		"marker in tool output only":  "",
		"marker in stderr after turn": codexRequiredStartupStderr,
	} {
		t.Run(name, func(t *testing.T) {
			runner := &codexStartupRunner{steps: []codexStartupStep{{
				stdout: codexToolOutputQuotingMarker,
				stderr: stderr,
				err:    errors.New("harness: run [codex exec]: exit status 1"),
			}}}
			reports, err := runCodexForRequiredMCP(t, runner)
			if err == nil || errors.Is(err, errRequiredMCPUnavailable) {
				t.Fatalf("Run error = %v, want an unclassified failure", err)
			}
			if !slices.Equal(reports, codexStartedSessionReports) {
				t.Fatalf("readiness reports = %+v, want %+v", reports, codexStartedSessionReports)
			}
		})
	}
}

// TestCodexResumeStartupFailureIsNotClassified confirms a completion-repair
// resume that the CLI refuses to start keeps the ordinary harness error: the
// model already ran in the first invocation, so it is not a before-model
// fault, and no transport_failure is recorded for it.
func TestCodexResumeStartupFailureIsNotClassified(t *testing.T) {
	runner := &codexStartupRunner{steps: []codexStartupStep{
		{stdout: codexCompletedStream},
		{stderr: codexRequiredStartupStderr, err: errors.New("harness: run [codex exec resume]: exit status 1")},
	}}
	reports, err := runCodexForRequiredMCP(t, runner)
	if runner.calls != 2 {
		t.Fatalf("codex invocations = %d, want the initial turn and one repair resume", runner.calls)
	}
	if err == nil || errors.Is(err, errRequiredMCPUnavailable) {
		t.Fatalf("Run error = %v, want an unclassified failure", err)
	}
	if !slices.Equal(reports, codexStartedSessionReports) {
		t.Fatalf("readiness reports = %+v, want %+v", reports, codexStartedSessionReports)
	}
}
