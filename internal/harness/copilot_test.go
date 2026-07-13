package harness

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
)

// fakeProcessRunner is a scripted ProcessRunner double: it lets tests inspect
// the built command/env/dir and script an arbitrary side effect (e.g. writing
// the completion file, as a real CLI would) without a real subprocess.
type fakeProcessRunner struct {
	lastReq ProcessRequest
	act     func(req ProcessRequest) error
	result  ProcessResult
	err     error
}

func (f *fakeProcessRunner) Run(ctx context.Context, req ProcessRequest) (ProcessResult, error) {
	f.lastReq = req
	if f.act != nil {
		if err := f.act(req); err != nil {
			return f.result, err
		}
	}
	return f.result, f.err
}

// pushCredentials builds a *credentials.Set materialized for "repo:push",
// backed by a real env-var token ref, for tests exercising credential
// injection into the CLI subprocess.
func pushCredentials(t *testing.T, capability, token string) *credentials.Set {
	t.Helper()
	t.Setenv("PUSH_TOKEN_ENV", token)
	resolver, err := credentials.NewResolver([]credentials.TokenRef{{Name: "push-ref", Env: "PUSH_TOKEN_ENV"}})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	injector, err := credentials.NewInjector(resolver, []credentials.Grant{{Capability: capability, Ref: "push-ref"}}, noopRegistrar{})
	if err != nil {
		t.Fatalf("NewInjector: %v", err)
	}
	set, err := injector.Materialize(context.Background(), []string{capability})
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	return set
}

func TestCopilotAdapterRendersPromptAndCollectsResult(t *testing.T) {
	workspace := t.TempDir()
	runner := &fakeProcessRunner{
		result: ProcessResult{Transcript: []byte("copilot: implementing...\ncopilot: done."), ExitCode: 0},
		act: func(req ProcessRequest) error {
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "ok"})
		},
	}
	adapter := &CopilotAdapter{
		Command:         []string{"copilot"},
		Runner:          runner,
		EnvCapabilities: map[string]string{"repo:push": "GH_TOKEN"},
	}

	creds := pushCredentials(t, "repo:push", "push-token-value")
	env := testEnvelope(workspace, "repo:push")
	req := RunRequest{
		Mode:           ModeInvoke,
		Envelope:       env,
		Instructions:   "You are a coder.",
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Credentials:    creds,
		Timeout:        5 * time.Second,
	}

	out, err := adapter.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(out.Payload) == 0 {
		t.Fatal("expected a non-empty result payload")
	}
	if string(out.Transcript) != "copilot: implementing...\ncopilot: done." {
		t.Fatalf("transcript = %q", out.Transcript)
	}

	// The command was built as Command + PromptFlag + prompt text + extras.
	if len(runner.lastReq.Command) < 3 || runner.lastReq.Command[0] != "copilot" || runner.lastReq.Command[1] != defaultPromptFlag {
		t.Fatalf("unexpected command: %v", runner.lastReq.Command)
	}
	promptText := runner.lastReq.Command[2]
	if !strings.Contains(promptText, "You are a coder.") {
		t.Fatalf("prompt missing instructions: %q", promptText)
	}
	if !strings.Contains(promptText, req.Envelope.Goal) {
		t.Fatalf("prompt missing goal: %q", promptText)
	}
	if !strings.Contains(promptText, DefaultResultPath) {
		t.Fatalf("prompt missing completion path directive: %q", promptText)
	}
	found := false
	for _, arg := range runner.lastReq.Command {
		if arg == "--allow-all-tools" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected --allow-all-tools in default extra args: %v", runner.lastReq.Command)
	}
	// The prompt is also written to the workspace for human debugging.
	debugPrompt, err := os.ReadFile(filepath.Join(workspace, ".goobers", "prompt.md"))
	if err != nil {
		t.Fatalf("read debug prompt: %v", err)
	}
	if string(debugPrompt) != promptText {
		t.Fatalf("debug prompt file does not match the prompt sent to the CLI")
	}

	// The credential was injected as an env var, not a CLI arg.
	foundEnv := false
	for _, kv := range runner.lastReq.Env {
		if kv == "GH_TOKEN=push-token-value" {
			foundEnv = true
		}
	}
	if !foundEnv {
		t.Fatalf("expected GH_TOKEN=push-token-value in subprocess env, got %v", runner.lastReq.Env)
	}
	for _, arg := range runner.lastReq.Command {
		if strings.Contains(arg, "push-token-value") {
			t.Fatalf("token leaked into argv: %v", runner.lastReq.Command)
		}
	}
}

func TestCopilotAdapterUndeclaredCapabilityNeverResolved(t *testing.T) {
	workspace := t.TempDir()
	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0},
		act: func(req ProcessRequest) error {
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	adapter := &CopilotAdapter{
		Command:         []string{"copilot"},
		Runner:          runner,
		EnvCapabilities: map[string]string{"repo:push": "GH_TOKEN"},
	}

	// Credentials materialized for "repo:read" only — "repo:push" was never
	// declared, so the adapter must not (and per credentials.Set, cannot)
	// resolve or inject it.
	creds := pushCredentials(t, "repo:read", "irrelevant")
	env := testEnvelope(workspace, "repo:read")
	req := RunRequest{
		Envelope:       env,
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Credentials:    creds,
	}
	if _, err := adapter.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, kv := range runner.lastReq.Env {
		if strings.HasPrefix(kv, "GH_TOKEN=") {
			t.Fatalf("undeclared capability's token leaked into env: %v", runner.lastReq.Env)
		}
	}
}

func TestCopilotAdapterFailsClosedOnMissingCommand(t *testing.T) {
	adapter := &CopilotAdapter{}
	if err := adapter.Preflight(context.Background()); err == nil {
		t.Fatal("expected Preflight to fail with no command configured")
	}
	_, err := adapter.Run(context.Background(), RunRequest{Workspace: t.TempDir(), CompletionPath: DefaultResultPath})
	if err == nil {
		t.Fatal("expected Run to fail with no command configured")
	}
}

func TestCopilotAdapterPreflightMissingBinary(t *testing.T) {
	adapter := &CopilotAdapter{Command: []string{"definitely-not-a-real-copilot-cli-binary"}}
	err := adapter.Preflight(context.Background())
	if err == nil {
		t.Fatal("expected Preflight to fail for a binary not on PATH")
	}
	if !strings.Contains(err.Error(), "not found on PATH") {
		t.Fatalf("error = %v, want an actionable PATH message", err)
	}
}

func TestCopilotAdapterPreflightSucceeds(t *testing.T) {
	runner := &fakeProcessRunner{result: ProcessResult{ExitCode: 0}}
	adapter := &CopilotAdapter{Command: []string{"echo"}, Runner: runner}
	if err := adapter.Preflight(context.Background()); err != nil {
		t.Fatalf("Preflight: %v", err)
	}
}

func TestCopilotAdapterPreflightNonZeroExit(t *testing.T) {
	runner := &fakeProcessRunner{result: ProcessResult{ExitCode: 1}}
	adapter := &CopilotAdapter{Command: []string{"echo"}, Runner: runner}
	err := adapter.Preflight(context.Background())
	if err == nil {
		t.Fatal("expected Preflight to fail on non-zero exit")
	}
}

func TestExecProcessRunnerTimeout(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep not available")
	}
	runner := ExecProcessRunner{}
	_, err := runner.Run(context.Background(), ProcessRequest{
		Command: []string{"sleep", "5"},
		Timeout: 50 * time.Millisecond,
	})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("error = %v, want ErrTimeout", err)
	}
}

func TestExecProcessRunnerCapturesTranscript(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	runner := ExecProcessRunner{}
	res, err := runner.Run(context.Background(), ProcessRequest{
		Command: []string{"sh", "-c", "echo hello-stdout; echo hello-stderr 1>&2"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(string(res.Transcript), "hello-stdout") || !strings.Contains(string(res.Transcript), "hello-stderr") {
		t.Fatalf("transcript = %q", res.Transcript)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", res.ExitCode)
	}
}

// TestCopilotAdapterLiveSmoke performs a trivial edit task against a fixture
// workspace using the real, installed Copilot CLI. Skipped unless both
// GOOBERS_COPILOT_LIVE_SMOKE=1 is set and a `copilot` binary is on PATH — CI
// exercises the fake-harness path instead; this is the acceptance
// criterion's "Live smoke test behind an env flag."
func TestCopilotAdapterLiveSmoke(t *testing.T) {
	if os.Getenv("GOOBERS_COPILOT_LIVE_SMOKE") != "1" {
		t.Skip("set GOOBERS_COPILOT_LIVE_SMOKE=1 to run against a real, signed-in Copilot CLI")
	}
	if _, err := exec.LookPath("copilot"); err != nil {
		t.Skip("copilot CLI not found on PATH")
	}

	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "GREETING.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("seed fixture file: %v", err)
	}

	adapter := &CopilotAdapter{Command: []string{"copilot"}}
	if err := adapter.Preflight(context.Background()); err != nil {
		t.Fatalf("Preflight: %v", err)
	}

	resolver, err := credentials.NewResolver(nil)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	injector, err := credentials.NewInjector(resolver, nil, noopRegistrar{})
	if err != nil {
		t.Fatalf("NewInjector: %v", err)
	}
	creds, err := injector.Materialize(context.Background(), nil)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	env := testEnvelope(workspace)
	env.Goal = "Append the word 'world' to GREETING.txt, then write your result envelope as instructed."
	req := RunRequest{
		Mode:           ModeInvoke,
		Envelope:       env,
		Instructions:   "You are a coder goober performing a trivial smoke-test edit.",
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Credentials:    creds,
		Timeout:        2 * time.Minute,
	}
	out, err := adapter.Run(context.Background(), req)
	if err != nil {
		t.Fatalf("Run: %v (transcript: %s)", err, out.Transcript)
	}
	if len(out.Payload) == 0 {
		t.Fatal("expected a completion payload from the live CLI")
	}
}
