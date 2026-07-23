package harness

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/procenv"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
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

func testHarnessOptions(t *testing.T, values map[string]interface{}) map[string]apiextensionsv1.JSON {
	t.Helper()
	options := make(map[string]apiextensionsv1.JSON, len(values))
	for name, value := range values {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("marshal harness option %q: %v", name, err)
		}
		options[name] = apiextensionsv1.JSON{Raw: raw}
	}
	return options
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

// twoTokenCredentials materializes a *credentials.Set granting two distinct
// capabilities from two distinct token refs — the multi-token case #288 wires,
// where a stage holds a personal Copilot-Requests PAT for the model alongside
// an org-repo token for the github tool.
func twoTokenCredentials(t *testing.T, capA, tokA, capB, tokB string) *credentials.Set {
	t.Helper()
	t.Setenv("TOK_A_ENV", tokA)
	t.Setenv("TOK_B_ENV", tokB)
	resolver, err := credentials.NewResolver([]credentials.TokenRef{
		{Name: "ref-a", Env: "TOK_A_ENV"},
		{Name: "ref-b", Env: "TOK_B_ENV"},
	})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	injector, err := credentials.NewInjector(resolver, []credentials.Grant{
		{Capability: capA, Ref: "ref-a"},
		{Capability: capB, Ref: "ref-b"},
	}, noopRegistrar{})
	if err != nil {
		t.Fatalf("NewInjector: %v", err)
	}
	set, err := injector.Materialize(context.Background(), []string{capA, capB})
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	return set
}

// TestCopilotAdapterInjectsModelAndGitHubTokensTogether is #288's core property
// (§3.3): a stage declaring both agent:model and an org-repo capability carries
// BOTH tokens into one subprocess under DISTINCT env vars — the model token as
// COPILOT_GITHUB_TOKEN (which the Copilot CLI prefers for model auth) and the
// github-tool token as GH_TOKEN — so neither clobbers the other. This is the
// two-tokens-one-subprocess case the agentic curate stage needs at #30.
func TestCopilotAdapterInjectsModelAndGitHubTokensTogether(t *testing.T) {
	workspace := t.TempDir()
	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0},
		act: func(req ProcessRequest) error {
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	adapter := &CopilotAdapter{
		Command: []string{"copilot"},
		Runner:  runner,
		EnvCapabilities: map[string]string{
			"agent:model":         "COPILOT_GITHUB_TOKEN",
			"github:issues:write": "GH_TOKEN",
		},
	}
	creds := twoTokenCredentials(t, "agent:model", "copilot-pat", "github:issues:write", "org-repo-token")
	req := RunRequest{
		Envelope:       testEnvelope(workspace, "agent:model", "github:issues:write"),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Credentials:    creds,
	}
	if _, err := adapter.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}
	gotModel, gotGitHub := false, false
	for _, kv := range runner.lastReq.Env {
		switch kv {
		case "COPILOT_GITHUB_TOKEN=copilot-pat":
			gotModel = true
		case "GH_TOKEN=org-repo-token":
			gotGitHub = true
		}
	}
	if !gotModel || !gotGitHub {
		t.Fatalf("expected both COPILOT_GITHUB_TOKEN=copilot-pat and GH_TOKEN=org-repo-token in one subprocess env, got %v", runner.lastReq.Env)
	}
	for _, arg := range runner.lastReq.Command {
		if strings.Contains(arg, "copilot-pat") || strings.Contains(arg, "org-repo-token") {
			t.Fatalf("token leaked into argv: %v", runner.lastReq.Command)
		}
	}
}

func TestCopilotAdapterDoesNotUseAnotherGoobersGrantWhenStoredAuthIsAllowed(t *testing.T) {
	t.Setenv("OTHER_GOOBER_TOKEN", "other-goober-token")
	resolver, err := credentials.NewResolver([]credentials.TokenRef{
		{Name: "other-goober", Env: "OTHER_GOOBER_TOKEN"},
	})
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	injector, err := credentials.NewGooberInjector(resolver, "goober-a", []credentials.Grant{
		{Goober: "goober-b", Capability: "agent:model", Ref: "other-goober"},
	}, noopRegistrar{})
	if err != nil {
		t.Fatalf("NewGooberInjector: %v", err)
	}
	creds, err := injector.Materialize(context.Background(), []string{"agent:model"})
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	workspace := t.TempDir()
	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0},
		act: func(req ProcessRequest) error {
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	adapter := &CopilotAdapter{
		Command:                        []string{"copilot"},
		Runner:                         runner,
		EnvCapabilities:                map[string]string{"agent:model": "COPILOT_GITHUB_TOKEN"},
		OptionalCredentialCapabilities: map[string]bool{"agent:model": true},
	}
	_, err = adapter.Run(context.Background(), RunRequest{
		Envelope:       testEnvelope(workspace, "agent:model"),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Credentials:    creds,
	})
	if err != nil {
		t.Fatalf("Run with stored auth fallback: %v", err)
	}
	for _, entry := range runner.lastReq.Env {
		if entry == "COPILOT_GITHUB_TOKEN=other-goober-token" {
			t.Fatalf("another goober's grant leaked into subprocess env: %v", runner.lastReq.Env)
		}
	}
}

func TestCopilotAdapterUsesStoredAuthWhenAgentModelGrantIsAbsent(t *testing.T) {
	t.Setenv("USERPROFILE", `C:\Users\operator`)
	resolver, err := credentials.NewResolver(nil)
	if err != nil {
		t.Fatal(err)
	}
	injector, err := credentials.NewGooberInjector(resolver, "goober-a", nil, noopRegistrar{})
	if err != nil {
		t.Fatal(err)
	}
	creds, err := injector.Materialize(context.Background(), []string{"agent:model"})
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0},
		act: func(req ProcessRequest) error {
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	adapter := &CopilotAdapter{
		Command:                        []string{"copilot"},
		Runner:                         runner,
		EnvCapabilities:                map[string]string{"agent:model": "COPILOT_GITHUB_TOKEN"},
		OptionalCredentialCapabilities: map[string]bool{"agent:model": true},
	}
	if _, err := adapter.Run(context.Background(), RunRequest{
		Envelope:       testEnvelope(workspace, "agent:model"),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Credentials:    creds,
	}); err != nil {
		t.Fatalf("Run with stored auth: %v", err)
	}
	hasProfile := false
	for _, entry := range runner.lastReq.Env {
		if entry == `USERPROFILE=C:\Users\operator` {
			hasProfile = true
		}
	}
	if !hasProfile {
		t.Fatalf("stored-auth profile location missing from env: %v", runner.lastReq.Env)
	}
	for _, entry := range runner.lastReq.Env {
		if strings.HasPrefix(entry, "COPILOT_GITHUB_TOKEN=") {
			t.Fatalf("unexpected model token injected during stored auth: %v", runner.lastReq.Env)
		}
	}
}

func TestCopilotAdapterStillFailsClosedForMissingRequiredCredential(t *testing.T) {
	resolver, err := credentials.NewResolver(nil)
	if err != nil {
		t.Fatal(err)
	}
	injector, err := credentials.NewGooberInjector(resolver, "goober-a", nil, noopRegistrar{})
	if err != nil {
		t.Fatal(err)
	}
	creds, err := injector.Materialize(context.Background(), []string{"github:issues:write"})
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeProcessRunner{}
	adapter := &CopilotAdapter{
		Command:         []string{"copilot"},
		Runner:          runner,
		EnvCapabilities: map[string]string{"github:issues:write": "GH_TOKEN"},
	}
	_, err = adapter.Run(context.Background(), RunRequest{
		Envelope:       testEnvelope(t.TempDir(), "github:issues:write"),
		Workspace:      t.TempDir(),
		CompletionPath: DefaultResultPath,
		Credentials:    creds,
	})
	if !errors.Is(err, credentials.ErrNoCredentialForCapability) {
		t.Fatalf("Run error = %v, want ErrNoCredentialForCapability", err)
	}
	if len(runner.lastReq.Command) != 0 {
		t.Fatalf("subprocess ran without required credential: %+v", runner.lastReq)
	}
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

	// The command contains PromptFlag + prompt text + extras. On Windows the
	// base command is the PowerShell npm shim rather than bare "copilot".
	promptIndex := slices.Index(runner.lastReq.Command, defaultPromptFlag)
	if promptIndex < 0 || promptIndex+1 >= len(runner.lastReq.Command) {
		t.Fatalf("unexpected command: %v", runner.lastReq.Command)
	}
	promptText := runner.lastReq.Command[promptIndex+1]
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
	telemetryPrefix := "GOOBERS_TELEMETRY_DIR="
	foundTelemetryDir := false
	for _, kv := range runner.lastReq.Env {
		if strings.HasPrefix(kv, telemetryPrefix) {
			foundTelemetryDir = true
			if info, err := os.Stat(strings.TrimPrefix(kv, telemetryPrefix)); err != nil || !info.IsDir() {
				t.Fatalf("telemetry dir is not writable stage storage: %q (%v)", kv, err)
			}
		}
	}
	if !foundTelemetryDir {
		t.Fatalf("expected GOOBERS_TELEMETRY_DIR in subprocess env, got %v", runner.lastReq.Env)
	}
	for _, arg := range runner.lastReq.Command {
		if strings.Contains(arg, "push-token-value") {
			t.Fatalf("token leaked into argv: %v", runner.lastReq.Command)
		}
	}
}

func TestCopilotAdapterRecoversMissingCompletionInSameSession(t *testing.T) {
	for _, tc := range []struct {
		name           string
		mode           Mode
		completionPath string
		completion     interface{}
	}{
		{
			name:           "result",
			mode:           ModeInvoke,
			completionPath: DefaultResultPath,
			completion:     apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "done"},
		},
		{
			name:           "verdict",
			mode:           ModeReview,
			completionPath: DefaultVerdictPath,
			completion:     apiv1.Verdict{Decision: apiv1.VerdictPass, Summary: "approved"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", "")
			t.Setenv("COPILOT_HOME", "")
			workspace := t.TempDir()
			var calls []ProcessRequest
			runner := &fakeProcessRunner{
				result: ProcessResult{Transcript: []byte("finished"), ExitCode: 0},
				act: func(req ProcessRequest) error {
					calls = append(calls, req)
					if len(calls) == 2 {
						return WriteCompletion(req.Dir, tc.completionPath, tc.completion)
					}
					return nil
				},
			}
			adapter := &CopilotAdapter{Command: []string{"copilot"}, Runner: runner}

			out, err := adapter.Run(context.Background(), RunRequest{
				Mode:           tc.mode,
				Envelope:       testEnvelope(workspace),
				Workspace:      workspace,
				CompletionPath: tc.completionPath,
				Timeout:        time.Minute,
			})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(out.Payload) == 0 {
				t.Fatal("recovery produced no completion payload")
			}
			if got := strings.Count(string(out.Transcript), "finished"); got != 2 {
				t.Fatalf("recovery transcript preserved %d process outputs, want 2: %q", got, out.Transcript)
			}
			if len(calls) != 2 {
				t.Fatalf("process calls = %d, want initial call plus one recovery", len(calls))
			}
			firstSession, firstOK := nativeSessionID(calls[0])
			secondSession, secondOK := nativeSessionID(calls[1])
			if !firstOK || !secondOK || firstSession != secondSession {
				t.Fatalf("recovery did not resume the initial session: first=%q second=%q", firstSession, secondSession)
			}
			promptIndex := slices.Index(calls[1].Command, defaultPromptFlag)
			if promptIndex < 0 || promptIndex+1 >= len(calls[1].Command) {
				t.Fatalf("recovery command missing prompt: %v", calls[1].Command)
			}
			recoveryPrompt := calls[1].Command[promptIndex+1]
			if !strings.Contains(recoveryPrompt, tc.completionPath) ||
				!strings.Contains(recoveryPrompt, "ended without writing the mandatory completion file") {
				t.Fatalf("recovery prompt = %q", recoveryPrompt)
			}
			if calls[1].Timeout <= 0 || calls[1].Timeout >= calls[0].Timeout {
				t.Fatalf("recovery timeout = %s, want positive remainder of %s", calls[1].Timeout, calls[0].Timeout)
			}
		})
	}
}

func TestCopilotAdapterPersistentMissingCompletionStopsAfterOneRecovery(t *testing.T) {
	workspace := t.TempDir()
	calls := 0
	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0},
		act: func(ProcessRequest) error {
			calls++
			return nil
		},
	}
	adapter := &CopilotAdapter{Command: []string{"copilot"}, Runner: runner}

	_, err := adapter.Run(context.Background(), RunRequest{
		Mode:           ModeInvoke,
		Envelope:       testEnvelope(workspace),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Timeout:        time.Minute,
	})
	if !errors.Is(err, ErrNoCompletion) {
		t.Fatalf("Run error = %v, want ErrNoCompletion", err)
	}
	if calls != 2 {
		t.Fatalf("process calls = %d, want exactly one bounded recovery", calls)
	}
}

func TestMergeProcessResultsPreservesRecoveryAndDroppedByteAccounting(t *testing.T) {
	const limit = int64(10)
	for _, tc := range []struct {
		name        string
		first       ProcessResult
		second      ProcessResult
		wantText    string
		wantDropped int64
	}{
		{
			name: "truncated initial turn",
			first: ProcessResult{
				Transcript:             append([]byte("abcdefghij"), transcriptTruncationMarker(5)...),
				TranscriptTruncated:    true,
				TranscriptDroppedBytes: 5,
			},
			second:      ProcessResult{Transcript: []byte("RECOVER")},
			wantText:    "ab\nRECOVER\n[transcript truncated: 13 bytes dropped]\n",
			wantDropped: 13,
		},
		{
			name:  "truncated recovery turn",
			first: ProcessResult{Transcript: []byte("initial")},
			second: ProcessResult{
				Transcript:             append([]byte("RECOVERY!!"), transcriptTruncationMarker(4)...),
				TranscriptTruncated:    true,
				TranscriptDroppedBytes: 4,
			},
			wantText:    "RECOVERY!!\n[transcript truncated: 11 bytes dropped]\n",
			wantDropped: 11,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := mergeProcessResults(tc.first, tc.second, limit)
			if string(got.Transcript) != tc.wantText {
				t.Fatalf("Transcript = %q, want %q", got.Transcript, tc.wantText)
			}
			if !got.TranscriptTruncated {
				t.Fatal("TranscriptTruncated = false, want true")
			}
			if got.TranscriptDroppedBytes != tc.wantDropped {
				t.Fatalf("TranscriptDroppedBytes = %d, want %d", got.TranscriptDroppedBytes, tc.wantDropped)
			}
		})
	}
}

func TestCopilotAdapterValidatesConfigAndBuildsArguments(t *testing.T) {
	adapter := &CopilotAdapter{}
	for _, tc := range []struct {
		name    string
		model   string
		options map[string]apiextensionsv1.JSON
		wantErr string
	}{
		{name: "valid", model: "claude-sonnet-5", options: testHarnessOptions(t, map[string]interface{}{"context": "long_context", "reasoningEffort": "xhigh"})},
		{name: "default context supported", model: "claude-sonnet-4.5", options: testHarnessOptions(t, map[string]interface{}{"context": "default"})},
		{name: "fable model supported", model: "claude-fable-5"},
		{name: "fast opus model supported", model: "claude-opus-4.8-fast"},
		{name: "opus 4.5 model supported", model: "claude-opus-4.5"},
		{name: "kimi model supported", model: "kimi-k2.7-code"},
		{name: "mai CLI model supported", model: "mai-code-1-flash"},
		{name: "non-CLI model alias rejected", model: "mai-code-1-flash-picker", wantErr: "unknown model"},
		{name: "unknown model", model: "not-a-model", wantErr: "unknown model"},
		{name: "unknown option", options: testHarnessOptions(t, map[string]interface{}{"temperature": "0.2"}), wantErr: "unknown harness option"},
		{name: "invalid option type", model: "claude-sonnet-5", options: testHarnessOptions(t, map[string]interface{}{"context": true}), wantErr: "must be a string"},
		{name: "unknown context value", model: "claude-sonnet-5", options: testHarnessOptions(t, map[string]interface{}{"context": "extended"}), wantErr: "invalid context"},
		{name: "long context unsupported", model: "claude-sonnet-4.5", options: testHarnessOptions(t, map[string]interface{}{"context": "long_context"}), wantErr: "not supported"},
		{name: "reasoning unsupported", model: "claude-sonnet-4.5", options: testHarnessOptions(t, map[string]interface{}{"reasoningEffort": "high"}), wantErr: "not supported"},
		{name: "reasoning level unsupported", model: "claude-sonnet-4.6", options: testHarnessOptions(t, map[string]interface{}{"reasoningEffort": "xhigh"}), wantErr: "not supported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := adapter.ValidateConfig(tc.model, tc.options)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateConfig: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ValidateConfig error = %v, want %q", err, tc.wantErr)
			}
		})
	}

	workspace := t.TempDir()
	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0},
		act: func(req ProcessRequest) error {
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	adapter = &CopilotAdapter{Command: []string{"copilot"}, ExtraArgs: []string{}, Runner: runner}
	req := RunRequest{
		Envelope:       testEnvelope(workspace),
		Model:          "claude-sonnet-5",
		HarnessOptions: testHarnessOptions(t, map[string]interface{}{"context": "long_context", "reasoningEffort": "xhigh"}),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
	}
	if _, err := adapter.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}
	command := strings.Join(runner.lastReq.Command, " ")
	for _, want := range []string{
		"--model claude-sonnet-5",
		"--context long_context",
		"--reasoning-effort xhigh",
	} {
		if !strings.Contains(command, want) {
			t.Errorf("command = %q, want %q", command, want)
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

// TestCopilotAdapterDoesNotPassthroughAmbientDaemonEnv is the regression test
// for the QA finding on PR #70: the subprocess must not inherit the daemon
// process's own environment wholesale (os.Environ()), since that would leak
// any resolver-sourced credential env var (e.g. instance.yaml's
// token.env — GOOBERS_GITHUB_TOKEN) into every stage regardless of whether it
// declared the corresponding capability (SEC-045, GBO-052).
func TestCopilotAdapterDoesNotPassthroughAmbientDaemonEnv(t *testing.T) {
	const ambientSecretVar = "GOOBERS_GITHUB_TOKEN"
	t.Setenv(ambientSecretVar, "ambient-daemon-secret-never-declared")

	workspace := t.TempDir()
	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0},
		act: func(req ProcessRequest) error {
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	adapter := &CopilotAdapter{Command: []string{"copilot"}, Runner: runner}

	// No capabilities declared at all — the stage asked for nothing.
	env := testEnvelope(workspace)
	req := RunRequest{
		Envelope:       env,
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Credentials:    pushCredentials(t, "unused", "unused"),
	}
	if _, err := adapter.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, kv := range runner.lastReq.Env {
		if strings.HasPrefix(kv, ambientSecretVar+"=") {
			t.Fatalf("ambient daemon env var leaked into subprocess env: %v", runner.lastReq.Env)
		}
	}
	// The allowlist itself (PATH at minimum) should still be present, so the
	// fix isn't accidentally starving the CLI of what it needs to run.
	foundPath := false
	for _, kv := range runner.lastReq.Env {
		if strings.HasPrefix(kv, "PATH=") {
			foundPath = true
		}
	}
	if !foundPath {
		t.Fatalf("expected PATH to still be passed through via the allowlist, got %v", runner.lastReq.Env)
	}
}

// TestCopilotAdapterPassesThroughExtendedAllowlist is the regression test for
// #75: the well-known, non-secret env conventions diverse tier-1 hosts rely
// on (XDG base dirs, locale, TLS/proxy config) must reach the subprocess, not
// just PATH/HOME/TMPDIR.
func TestCopilotAdapterPassesThroughExtendedAllowlist(t *testing.T) {
	extended := map[string]string{
		"XDG_CONFIG_HOME": "/home/tester/.config",
		"XDG_DATA_HOME":   "/home/tester/.local/share",
		"LANG":            "en_US.UTF-8",
		"LC_ALL":          "C",
		"LC_CTYPE":        "en_US.UTF-8",
		"SSL_CERT_FILE":   "/etc/ssl/certs/custom-ca.pem",
		"HTTP_PROXY":      "http://proxy.example.internal:8080",
		"HTTPS_PROXY":     "https://proxy.example.internal:8443",
		"NO_PROXY":        "localhost,127.0.0.1",
	}
	for name, value := range extended {
		t.Setenv(name, value)
	}

	workspace := t.TempDir()
	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0},
		act: func(req ProcessRequest) error {
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	adapter := &CopilotAdapter{Command: []string{"copilot"}, Runner: runner}

	req := RunRequest{
		Envelope:       testEnvelope(workspace),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Credentials:    pushCredentials(t, "unused", "unused"),
	}
	if _, err := adapter.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := make(map[string]bool, len(extended))
	for name, value := range extended {
		want := name + "=" + value
		for _, kv := range runner.lastReq.Env {
			if kv == want {
				got[name] = true
			}
		}
	}
	for name := range extended {
		if !got[name] {
			t.Fatalf("%s did not pass through into subprocess env, got %v", name, runner.lastReq.Env)
		}
	}
}

// TestCopilotAdapterExtendedAllowlistStillBlocksSecretShapedVars proves the
// #75 extension stays default-deny: an ambient var that merely resembles an
// allowlisted name (shares a prefix substring) or looks like a credential
// must not pass, only exact allowlisted names and the LC_* family do.
func TestCopilotAdapterExtendedAllowlistStillBlocksSecretShapedVars(t *testing.T) {
	blocked := map[string]string{
		// Secret-shaped, unrelated to the allowlist.
		"AWS_SECRET_ACCESS_KEY": "not-a-real-secret-but-should-never-pass",
		// Shares the "LANG" substring as a prefix but is a distinct var name —
		// would leak if baseEnv used strings.HasPrefix(name, "LANG") instead
		// of an exact match.
		"LANGUAGE_MODEL_API_KEY": "should-not-pass-either",
		// Shares "LC_" as a substring but not as a prefix — must not match
		// the LC_* family.
		"LOCALE_LC_OVERRIDE_SECRET": "should-not-pass",
	}
	for name, value := range blocked {
		t.Setenv(name, value)
	}
	t.Setenv("LANG", "en_US.UTF-8") // the real, exact allowlisted name

	workspace := t.TempDir()
	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0},
		act: func(req ProcessRequest) error {
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	adapter := &CopilotAdapter{Command: []string{"copilot"}, Runner: runner}

	req := RunRequest{
		Envelope:       testEnvelope(workspace),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Credentials:    pushCredentials(t, "unused", "unused"),
	}
	if _, err := adapter.Run(context.Background(), req); err != nil {
		t.Fatalf("Run: %v", err)
	}

	for name := range blocked {
		for _, kv := range runner.lastReq.Env {
			if strings.HasPrefix(kv, name+"=") {
				t.Fatalf("blocked var %s leaked into subprocess env: %v", name, runner.lastReq.Env)
			}
		}
	}
	foundLang := false
	for _, kv := range runner.lastReq.Env {
		if kv == "LANG=en_US.UTF-8" {
			foundLang = true
		}
	}
	if !foundLang {
		t.Fatalf("expected the exact allowlisted LANG to still pass through, got %v", runner.lastReq.Env)
	}
}

// TestBaseEnvMatchesProcenv is the #248 drift-guard: harness's baseEnv()
// must be exactly procenv.BaseEnv() — the shared definition executor's
// baseEnv() also delegates to — not a local copy that can silently diverge
// again the way #98/#122 did.
func TestBaseEnvMatchesProcenv(t *testing.T) {
	t.Setenv("GOMODCACHE", "/custom/gomodcache")
	t.Setenv("LC_ALL", "C")

	got := append([]string(nil), baseEnv(nil)...)
	want := append([]string(nil), procenv.BaseEnv()...)
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("harness baseEnv() diverged from procenv.BaseEnv():\n got:  %v\n want: %v", got, want)
	}
}

// TestBaseEnvAppliesExtraAllowlist is #736's harness path: the adapter's
// ExtraEnvAllowlist (RunnerConfig.EnvPassthrough) reaches the harness
// subprocess env additively, staying default-deny for undeclared vars.
func TestBaseEnvAppliesExtraAllowlist(t *testing.T) {
	t.Setenv("MY_HARNESS_TOOLCHAIN", "/opt/harness-tool")
	t.Setenv("MY_HARNESS_UNDECLARED", "should-not-pass")

	env := baseEnv([]string{"MY_HARNESS_TOOLCHAIN"})
	found := false
	for _, kv := range env {
		if kv == "MY_HARNESS_TOOLCHAIN=/opt/harness-tool" {
			found = true
		}
		if strings.HasPrefix(kv, "MY_HARNESS_UNDECLARED=") {
			t.Fatalf("undeclared ambient var leaked into harness baseEnv: %v", env)
		}
	}
	if !found {
		t.Fatalf("extra-allowlisted var missing from harness baseEnv: %v", env)
	}
}

// TestCopilotAdapterEmptyWorkspaceIsConfigError is the regression test for
// #122: exec.Cmd treats Dir == "" as "run in the current process's working
// directory" — an unset RunRequest.Workspace must fail closed as a
// configuration error instead of silently running in the daemon's own cwd.
func TestCopilotAdapterEmptyWorkspaceIsConfigError(t *testing.T) {
	adapter := &CopilotAdapter{Command: []string{"copilot"}, Runner: &fakeProcessRunner{result: ProcessResult{ExitCode: 0}}}
	_, err := adapter.Run(context.Background(), RunRequest{CompletionPath: DefaultResultPath}) // Workspace left empty
	if err == nil {
		t.Fatal("expected an error for an empty Workspace")
	}
}

func TestCopilotAdapterFailsClosedOnMissingCommand(t *testing.T) {
	adapter := &CopilotAdapter{}
	if _, err := adapter.Preflight(context.Background()); err == nil {
		t.Fatal("expected Preflight to fail with no command configured")
	}
	_, err := adapter.Run(context.Background(), RunRequest{Workspace: t.TempDir(), CompletionPath: DefaultResultPath})
	if err == nil {
		t.Fatal("expected Run to fail with no command configured")
	}
}

func TestCopilotAdapterPreflightMissingBinary(t *testing.T) {
	adapter := &CopilotAdapter{Command: []string{"definitely-not-a-real-copilot-cli-binary"}}
	_, err := adapter.Preflight(context.Background())
	if err == nil {
		t.Fatal("expected Preflight to fail for a binary not on PATH")
	}
	if !strings.Contains(err.Error(), "not found on PATH") {
		t.Fatalf("error = %v, want an actionable PATH message", err)
	}
}

func TestCopilotAdapterPreflightSucceeds(t *testing.T) {
	runner := &fakeProcessRunner{result: ProcessResult{ExitCode: 0, Transcript: []byte("copilot version 1.2.3\n")}}
	adapter := &CopilotAdapter{Command: []string{"echo"}, Runner: runner}
	info, err := adapter.Preflight(context.Background())
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if info.Version != "copilot version 1.2.3" {
		t.Fatalf("Preflight version = %q", info.Version)
	}
}

func TestCopilotAdapterPreflightRequiresVersionOutput(t *testing.T) {
	adapter := &CopilotAdapter{
		Command: []string{"echo"},
		Runner:  &fakeProcessRunner{result: ProcessResult{ExitCode: 0}},
	}
	if _, err := adapter.Preflight(context.Background()); err == nil || !strings.Contains(err.Error(), "returned no version") {
		t.Fatalf("Preflight error = %v", err)
	}
}

func TestCopilotAdapterPreflightNonZeroExit(t *testing.T) {
	runner := &fakeProcessRunner{result: ProcessResult{ExitCode: 1}}
	adapter := &CopilotAdapter{Command: []string{"echo"}, Runner: runner}
	_, err := adapter.Preflight(context.Background())
	if err == nil {
		t.Fatal("expected Preflight to fail on non-zero exit")
	}
}

// TestCopilotAdapterRun_PassesMaxTranscriptBytesThrough confirms
// RunRequest.MaxTranscriptBytes reaches the underlying ProcessRequest, and
// Outcome carries back whatever the ProcessRunner reported — the plumbing
// #245 threads between the two layers.
func TestCopilotAdapterRun_PassesMaxTranscriptBytesThrough(t *testing.T) {
	workspace := t.TempDir()
	runner := &fakeProcessRunner{
		result: ProcessResult{TranscriptTruncated: true, TranscriptDroppedBytes: 42},
		act: func(req ProcessRequest) error {
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	adapter := &CopilotAdapter{Command: []string{"copilot"}, Runner: runner}
	out, err := adapter.Run(context.Background(), RunRequest{
		Workspace:          workspace,
		CompletionPath:     DefaultResultPath,
		MaxTranscriptBytes: 2048,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runner.lastReq.MaxTranscriptBytes != 2048 {
		t.Fatalf("ProcessRequest.MaxTranscriptBytes = %d, want 2048", runner.lastReq.MaxTranscriptBytes)
	}
	if !out.TranscriptTruncated || out.TranscriptDroppedBytes != 42 {
		t.Fatalf("Outcome = {%v, %d}, want {true, 42}", out.TranscriptTruncated, out.TranscriptDroppedBytes)
	}
}

// TestCopilotAdapterPreflightSignedOutFailsAuthProbe is the #238 control: a CLI
// that passes --version but fails the configured auth probe (signed out) fails
// preflight — the case a version-only check misses (GBO-011).
func TestCopilotAdapterPreflightSignedOutFailsAuthProbe(t *testing.T) {
	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0, Transcript: []byte("copilot version 1.2.3\n")}, // --version succeeds
		act: func(req ProcessRequest) error {
			for _, a := range req.Command {
				if a == "auth" { // the auth probe fails: signed out
					return errors.New("not signed in")
				}
			}
			return nil
		},
	}
	adapter := &CopilotAdapter{Command: []string{"echo"}, AuthCheckArgs: []string{"auth", "status"}, Runner: runner}
	_, err := adapter.Preflight(context.Background())
	if err == nil {
		t.Fatal("expected preflight to fail when the sign-in probe fails")
	}
	if !strings.Contains(err.Error(), "sign") {
		t.Fatalf("error should be an actionable sign-in message: %v", err)
	}
}

// TestCopilotAdapterPreflightSignedInPasses confirms preflight passes when both
// --version and the configured auth probe succeed.
func TestCopilotAdapterPreflightSignedInPasses(t *testing.T) {
	adapter := &CopilotAdapter{
		Command:       []string{"echo"},
		AuthCheckArgs: []string{"auth", "status"},
		Runner:        &fakeProcessRunner{result: ProcessResult{ExitCode: 0, Transcript: []byte("copilot version 1.2.3\n")}},
	}
	if _, err := adapter.Preflight(context.Background()); err != nil {
		t.Fatalf("preflight should pass when signed in: %v", err)
	}
}

// TestCopilotAdapterPreflightNoAuthProbeByDefault confirms that with no
// AuthCheckArgs configured, preflight does not run (or require) an auth probe —
// so the version-only path is unchanged until a real auth command is wired.
func TestCopilotAdapterPreflightNoAuthProbeByDefault(t *testing.T) {
	calls := 0
	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0, Transcript: []byte("copilot version 1.2.3\n")},
		act:    func(ProcessRequest) error { calls++; return nil },
	}
	adapter := &CopilotAdapter{Command: []string{"echo"}, Runner: runner} // no AuthCheckArgs
	if _, err := adapter.Preflight(context.Background()); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected exactly one probe (--version), got %d — no auth probe should run by default", calls)
	}
}
