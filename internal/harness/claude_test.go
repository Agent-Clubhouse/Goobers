package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/platform/secfile"
	"github.com/goobers/goobers/internal/telemetry"
)

const claudeResultStream = `{"type":"system","subtype":"init","model":"claude-sonnet-4-6","session_id":"session-1"}
{"type":"assistant","message":{"model":"claude-sonnet-4-6","content":[{"type":"text","text":"done"}]}}
{"type":"result","subtype":"success","result":"done","total_cost_usd":0.25,"usage":{"input_tokens":120,"output_tokens":30},"modelUsage":{"claude-sonnet-4-6":{"inputTokens":120,"outputTokens":30,"costUSD":0.25}}}
`

func TestClaudeAdapterPreservesLifecycleEvents(t *testing.T) {
	stubClaudeCredentialsHome(t)
	workspace := t.TempDir()
	var live []journal.Event
	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0, Transcript: []byte(claudeResultStream)},
		act: func(req ProcessRequest) error {
			if len(live) != 1 || live[0].Agent == nil || live[0].Agent.Lifecycle != journal.AgentStarted {
				return fmt.Errorf("started lifecycle was not emitted before process launch: %#v", live)
			}
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	adapter := &ClaudeAdapter{Command: []string{"claude-lifecycle-test"}, Runner: runner, KeychainReader: absentClaudeKeychain}
	out, err := adapter.Run(context.Background(), RunRequest{
		Envelope: testEnvelope(workspace), Workspace: workspace, CompletionPath: DefaultResultPath,
		AgentEventSink: func(event journal.Event) error {
			live = append(live, event)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertAdapterLifecycle(t, out, "claude")
	if len(live) != 2 || live[1].Agent == nil || live[1].Agent.Lifecycle != journal.AgentCompleted {
		t.Fatalf("live lifecycle = %#v", live)
	}
}

func TestClaudeAdapterRunUsesHeadlessContractAndScopedEnvironment(t *testing.T) {
	const ambientSecret = "GOOBERS_UNDECLARED_SECRET"
	t.Setenv(ambientSecret, "must-not-leak")
	workspace := t.TempDir()
	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0, Transcript: []byte(claudeResultStream)},
		act: func(req ProcessRequest) error {
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{
				Status:  apiv1.ResultSuccess,
				Summary: "implemented",
			})
		},
	}
	adapter := &ClaudeAdapter{
		KeychainReader: absentClaudeKeychain,
		Command:        []string{"claude-headless-test"},
		Runner:         runner,
		EnvCapabilities: map[string]string{
			"agent:model":         "ANTHROPIC_API_KEY",
			"github:issues:write": "GH_TOKEN",
		},
	}
	credentials := twoTokenCredentials(t,
		"agent:model", "sk-ant-test-key",
		"github:issues:write", "github-token",
	)

	out, err := adapter.Run(context.Background(), RunRequest{
		Envelope:       testEnvelope(workspace, "agent:model", "github:issues:write"),
		Model:          "claude-sonnet-4-6",
		HarnessOptions: testHarnessOptions(t, map[string]interface{}{"effort": "high"}),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Credentials:    credentials,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	command := runner.lastReq.Command
	for _, want := range []string{
		"claude-headless-test", "-p", "--output-format", "stream-json", "--verbose",
		"--permission-mode", "bypassPermissions", "--model", "claude-sonnet-4-6",
		"--effort", "high",
	} {
		if !slices.Contains(command, want) {
			t.Errorf("command missing %q: %v", want, command)
		}
	}
	prompt := string(runner.lastReq.Stdin)
	if !strings.Contains(prompt, "write your result as JSON") {
		t.Fatalf("stdin does not carry the completion-file prompt: %q", prompt)
	}
	if command[len(command)-2] != "--session-id" {
		t.Fatalf("argv carries a trailing positional prompt: %v", command)
	}
	for _, token := range []string{"sk-ant-test-key", "github-token"} {
		for _, arg := range command {
			if strings.Contains(arg, token) {
				t.Fatalf("credential leaked into argv: %v", command)
			}
		}
	}
	for _, want := range []string{"ANTHROPIC_API_KEY=sk-ant-test-key", "GH_TOKEN=github-token"} {
		if !slices.Contains(runner.lastReq.Env, want) {
			t.Errorf("environment missing %q: %v", want, runner.lastReq.Env)
		}
	}
	for _, entry := range runner.lastReq.Env {
		if strings.HasPrefix(entry, ambientSecret+"=") {
			t.Fatalf("ambient secret leaked into environment: %v", runner.lastReq.Env)
		}
	}
	if out.TranscriptSchema != telemetry.GenAIEventSchema {
		t.Fatalf("TranscriptSchema = %q, want %q", out.TranscriptSchema, telemetry.GenAIEventSchema)
	}
	if got := out.Metrics[telemetry.AttrGenAIUsageInputTokens]; got != 120 {
		t.Fatalf("input token metric = %v, want 120", got)
	}
	if got := out.Metrics[telemetry.AttrUsageCostUSD]; got != 0.25 {
		t.Fatalf("cost metric = %v, want 0.25", got)
	}
	if len(out.Payload) == 0 {
		t.Fatal("completion payload is empty")
	}
	assertAdapterLifecycle(t, out, "claude")
	if got := out.AgentEvents[0].Agent.RequestedReasoningEffort; got != "high" {
		t.Fatalf("requested reasoning effort = %q, want high", got)
	}
	if got := out.AgentEvents[0].Agent.ResolvedReasoningEffort; got != "high" {
		t.Fatalf("resolved reasoning effort = %q, want high", got)
	}
}

// TestClaudeAdapterRunDropsForeignShapedAnthropicAPIKey pins the fix for a
// mixed-harness instance where the credentialGrant schema allows only one
// agent:model grant per instance (no per-harness scoping): a grant backing
// Copilot's headless model auth (a GitHub PAT, never shaped like an
// Anthropic key) must not be passed through to claude-code as
// ANTHROPIC_API_KEY, since seedClaudeCredentials treats any non-empty value
// there as "caller already handled auth" and skips falling back to the
// user's real stored Claude Code session — turning a Copilot-only credential
// configuration into a guaranteed 401 for every claude-code invocation. A
// genuinely Anthropic-shaped key (sk-ant-...) must still pass through
// unchanged (see TestClaudeAdapterRunUsesHeadlessContractAndScopedEnvironment).
func TestClaudeAdapterRunDropsForeignShapedAnthropicAPIKey(t *testing.T) {
	stubClaudeCredentialsHome(t)
	workspace := t.TempDir()
	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0, Transcript: []byte(claudeResultStream)},
		act: func(req ProcessRequest) error {
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{
				Status:  apiv1.ResultSuccess,
				Summary: "implemented",
			})
		},
	}
	adapter := &ClaudeAdapter{
		KeychainReader: absentClaudeKeychain,
		Command:        []string{"claude"},
		Runner:         runner,
		EnvCapabilities: map[string]string{
			"agent:model": "ANTHROPIC_API_KEY",
		},
	}
	credentials := twoTokenCredentials(t,
		"agent:model", "github_pat_11AAcopilotPAT",
		"repo:read", "unused",
	)

	_, err := adapter.Run(context.Background(), RunRequest{
		Envelope:       testEnvelope(workspace, "agent:model"),
		Model:          "claude-sonnet-4-6",
		HarnessOptions: testHarnessOptions(t, map[string]interface{}{}),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Credentials:    credentials,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, entry := range runner.lastReq.Env {
		if strings.HasPrefix(entry, "ANTHROPIC_API_KEY=") {
			t.Fatalf("foreign-shaped credential must not reach ANTHROPIC_API_KEY: %v", runner.lastReq.Env)
		}
	}
}

// TestClaudeAdapterRunRoutesOAuthTokenToOAuthEnvVar pins #5148's Claude
// subscription OAuth token support: a resolved agent:model credential shaped
// like a `claude setup-token` OAuth token (sk-ant-oat...) must be injected as
// CLAUDE_CODE_OAUTH_TOKEN, never as ANTHROPIC_API_KEY — the API rejects an
// OAuth token sent as an API key.
func TestClaudeAdapterRunRoutesOAuthTokenToOAuthEnvVar(t *testing.T) {
	workspace := t.TempDir()
	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0, Transcript: []byte(claudeResultStream)},
		act: func(req ProcessRequest) error {
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	adapter := &ClaudeAdapter{
		KeychainReader: absentClaudeKeychain,
		Command:        []string{"claude"},
		Runner:         runner,
		EnvCapabilities: map[string]string{
			"agent:model": "ANTHROPIC_API_KEY",
		},
	}
	credentials := twoTokenCredentials(t,
		"agent:model", "sk-ant-oat01-test-oauth-token",
		"repo:read", "unused",
	)
	_, err := adapter.Run(context.Background(), RunRequest{
		Envelope:       testEnvelope(workspace, "agent:model"),
		Model:          "claude-sonnet-4-6",
		HarnessOptions: testHarnessOptions(t, map[string]interface{}{}),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Credentials:    credentials,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !slices.Contains(runner.lastReq.Env, "CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-test-oauth-token") {
		t.Fatalf("environment missing CLAUDE_CODE_OAUTH_TOKEN: %v", runner.lastReq.Env)
	}
	for _, entry := range runner.lastReq.Env {
		if strings.HasPrefix(entry, "ANTHROPIC_API_KEY=") {
			t.Fatalf("OAuth token must not also be injected as ANTHROPIC_API_KEY: %v", runner.lastReq.Env)
		}
	}
}

// TestRemapAnthropicOAuthToken unit-tests the env-rewrite in isolation: an
// OAuth-shaped ANTHROPIC_API_KEY moves to CLAUDE_CODE_OAUTH_TOKEN; a regular
// API key and any other entry pass through untouched.
func TestRemapAnthropicOAuthToken(t *testing.T) {
	in := []string{
		"PATH=/usr/bin",
		"ANTHROPIC_API_KEY=sk-ant-oat01-abc123",
		"GH_TOKEN=unrelated",
	}
	got := remapAnthropicOAuthToken(in)
	want := []string{
		"PATH=/usr/bin",
		"CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-abc123",
		"GH_TOKEN=unrelated",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("remapAnthropicOAuthToken(%v) = %v, want %v", in, got, want)
	}

	apiKeyIn := []string{"ANTHROPIC_API_KEY=sk-ant-api03-realkey"}
	apiKeyGot := remapAnthropicOAuthToken(apiKeyIn)
	if !slices.Equal(apiKeyGot, apiKeyIn) {
		t.Fatalf("remapAnthropicOAuthToken must not touch a non-OAuth-shaped API key: got %v, want %v", apiKeyGot, apiKeyIn)
	}
}

// TestSeedClaudeCredentialsSkipsWhenOAuthTokenInjected pins that the
// stored-credential seeding path (seedClaudeCredentialsForPlatform) treats an
// injected CLAUDE_CODE_OAUTH_TOKEN the same as a non-empty ANTHROPIC_API_KEY:
// "caller already handled auth", so it must not overwrite it by seeding the
// operator's own stored Claude Code session.
func TestSeedClaudeCredentialsSkipsWhenOAuthTokenInjected(t *testing.T) {
	destination := t.TempDir()
	env := []string{"CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-test-oauth-token"}
	called := false
	readKeychain := func(context.Context, string) ([]byte, error) {
		called = true
		return nil, errors.New("must not be consulted when auth is already handled")
	}
	if err := seedClaudeCredentialsForPlatform(context.Background(), env, destination, "darwin", readKeychain); err != nil {
		t.Fatalf("seedClaudeCredentialsForPlatform: %v", err)
	}
	if called {
		t.Fatal("keychain was consulted despite an injected CLAUDE_CODE_OAUTH_TOKEN")
	}
	if entries, _ := os.ReadDir(destination); len(entries) != 0 {
		t.Fatalf("destination should remain empty when auth was already handled, got %v", entries)
	}
}

// Claude 2.1.263 treats --model auto as an unrecognized literal model. The
// adapter admits auto as "let the harness pick", so its command must match
// the empty-model invocation while preserving explicit model selections.
func TestBuildClaudeArgvResolvesAutomaticModel(t *testing.T) {
	for _, model := range []string{"", "auto", "claude-sonnet-5"} {
		t.Run(model, func(t *testing.T) {
			adapter := &ClaudeAdapter{}
			if err := adapter.ValidateConfig(model, nil); err != nil {
				t.Fatal(err)
			}
			argv, sessionArg := buildClaudeArgv([]string{"claude"}, defaultClaudeExtraArgs, model, "high", "session-1")
			want := []string{
				"claude", "-p", "--output-format", "stream-json", "--verbose", "--permission-mode", "bypassPermissions",
				"--disallowedTools",
				"Bash(pkill:*)", "Bash(killall:*)",
				"Bash(/usr/bin/pkill:*)", "Bash(/usr/bin/killall:*)",
				"Bash(/bin/pkill:*)", "Bash(/bin/killall:*)",
				"Bash(/usr/bin/env pkill:*)", "Bash(/usr/bin/env killall:*)",
				"--setting-sources", "user",
			}
			if model == "claude-sonnet-5" {
				want = append(want, "--model", model)
			}
			want = append(want, "--effort", "high", "--session-id", "session-1")
			if !slices.Equal(argv, want) {
				t.Fatalf("command = %v, want %v", argv, want)
			}
			if argv[sessionArg] != "--session-id" || argv[sessionArg+1] != "session-1" {
				t.Fatalf("model selection broke recovery argument indices: %v", argv)
			}
		})
	}
}

// TestBuildClaudeArgvShiftUnderSandboxWrapping asserts sessionSelectorArg
// remains correct once confineArgv prepends sandbox wrapper arguments ahead
// of the whole command.
func TestBuildClaudeArgvShiftUnderSandboxWrapping(t *testing.T) {
	argv, sessionSelectorArg := buildClaudeArgv([]string{"claude"}, nil, "", "", "session-1")
	sandbox := &stubSandbox{}
	wrapped, shift, err := confineArgv(sandbox, argv, t.TempDir(), nil)
	if err != nil {
		t.Fatalf("confineArgv: %v", err)
	}
	sessionSelectorArg += shift
	if wrapped[sessionSelectorArg] != "--session-id" {
		t.Fatalf("shifted sessionSelectorArg = %d does not carry --session-id: %v", sessionSelectorArg, wrapped)
	}
}

func TestBuildClaudeArgvPinsUnattendedPermissionMode(t *testing.T) {
	for _, tc := range []struct {
		name        string
		base, extra []string
		want        string
	}{
		{name: "default unrestricted", extra: claudeExtraArgs(nil), want: "bypassPermissions"},
		{name: "tool allowlist", extra: claudeExtraArgs([]string{"Read"}), want: "dontAsk"},
		{name: "empty override", extra: []string{}, want: "dontAsk"},
		{name: "custom arguments", extra: []string{"--tools", "Read", "--allowedTools", "Read"}, want: "dontAsk"},
		{name: "explicit extra mode", extra: []string{"--permission-mode", "default"}, want: "default"},
		{name: "explicit command mode", base: []string{"claude", "--permission-mode", "plan"}, want: "plan"},
		{name: "equals mode", extra: []string{"--permission-mode=acceptEdits"}, want: "acceptEdits"},
		{name: "legacy skip", base: []string{"claude", "--dangerously-skip-permissions"}, want: "bypassPermissions"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := tc.base
			if base == nil {
				base = []string{"claude"}
			}
			argv, selector := buildClaudeArgv(base, tc.extra, "", "", "session-1")
			for _, resume := range []bool{false, true} {
				if resume {
					argv[selector] = "--resume"
				}
				var modes []string
				for i, arg := range argv {
					if arg == "--permission-mode" && i+1 < len(argv) {
						modes = append(modes, argv[i+1])
					} else if value, ok := strings.CutPrefix(arg, "--permission-mode="); ok {
						modes = append(modes, value)
					}
				}
				if !slices.Equal(modes, []string{tc.want}) {
					t.Fatalf("resume=%v: modes=%v, want %q; argv=%v", resume, modes, tc.want, argv)
				}
			}
		})
	}
}

type claudeSequenceRunner struct {
	reqs       []ProcessRequest
	results    []ProcessResult
	rawOutputs [][]byte
	acts       []func(ProcessRequest) error
}

func (r *claudeSequenceRunner) Run(_ context.Context, req ProcessRequest) (ProcessResult, error) {
	call := len(r.reqs)
	r.reqs = append(r.reqs, req)
	var result ProcessResult
	if call < len(r.rawOutputs) && r.rawOutputs[call] != nil {
		if req.StdoutCapture != nil {
			_, _ = req.StdoutCapture.Write(r.rawOutputs[call])
		}
		buf := newTranscriptBuffer(req.MaxTranscriptBytes)
		_, _ = buf.Write(r.rawOutputs[call])
		result = ProcessResult{
			ExitCode:               0,
			Transcript:             buf.Bytes(),
			TranscriptTruncated:    buf.Truncated(),
			TranscriptDroppedBytes: buf.Dropped(),
		}
	} else if call < len(r.results) {
		result = r.results[call]
	}
	if call < len(r.acts) && r.acts[call] != nil {
		if err := r.acts[call](req); err != nil {
			return ProcessResult{ExitCode: 1}, err
		}
	}
	return result, nil
}

// absentClaudeKeychain is the ClaudeKeychainReader every Run-level unit test
// injects (#7447): a Keychain with no Claude Code login, which is what a Mac
// that never signed in (every macOS CI runner) reports. Leaving
// ClaudeAdapter.KeychainReader nil under go test panics rather than reading
// the developer's real login Keychain.
func absentClaudeKeychain(context.Context, string) ([]byte, error) {
	return nil, ErrClaudeKeychainItemNotFound
}

// stubClaudeCredentialsHome points HOME at a fresh directory seeded with a
// stored credential, so an unsandboxed Run's unconditional credential-seeding
// copies a planted login instead of the developer's real ~/.claude one (which
// refuseHostClaudeConfigDirUnderTest turns into a panic), and never needs the
// Keychain fallback that TestSeedClaudeCredentialsReadsMacOSKeychainWhenFileMissing
// exercises with a fake reader.
func stubClaudeCredentialsHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	credentialDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(credentialDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(credentialDir, ".credentials.json"), []byte(`{"oauthToken":"stored-login"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

func TestClaudeAdapterPreservesTerminalResultBeyondTranscriptLimit(t *testing.T) {
	const limit = 512
	stubClaudeCredentialsHome(t)
	workspace := t.TempDir()
	raw := []byte(strings.Join([]string{
		`{"type":"system","subtype":"init","model":"claude-sonnet-4-6"}`,
		`{"type":"assistant","message":{"model":"claude-sonnet-4-6","content":[{"type":"text","text":"` + strings.Repeat("x", limit*2) + `"}]}}`,
		`{"type":"result","subtype":"success","result":"terminal output","total_cost_usd":0.25,"usage":{"input_tokens":120,"output_tokens":30},"modelUsage":{"claude-sonnet-4-6":{"inputTokens":120,"outputTokens":30,"costUSD":0.25}}}`,
	}, "\n"))
	runner := &claudeSequenceRunner{
		rawOutputs: [][]byte{raw},
		acts: []func(ProcessRequest) error{
			func(req ProcessRequest) error {
				return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
			},
		},
	}
	adapter := &ClaudeAdapter{Command: []string{"claude"}, Runner: runner, KeychainReader: absentClaudeKeychain}

	out, err := adapter.Run(context.Background(), RunRequest{
		Envelope:           testEnvelope(workspace),
		Workspace:          workspace,
		CompletionPath:     DefaultResultPath,
		MaxTranscriptBytes: limit,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := out.Metrics[telemetry.AttrGenAIUsageInputTokens]; got != 120 {
		t.Fatalf("input token metric = %v, want 120", got)
	}
	if len(out.ModelUsage) != 1 || out.ModelUsage[0].Model != "claude-sonnet-4-6" {
		t.Fatalf("model usage = %+v", out.ModelUsage)
	}
	if !bytes.Contains(out.Transcript, []byte("terminal output")) {
		t.Fatalf("transcript lost terminal output:\n%s", out.Transcript)
	}
	if !out.TranscriptTruncated || out.TranscriptDroppedBytes <= 0 {
		t.Fatalf("truncation = %v, dropped = %d; want truncated output", out.TranscriptTruncated, out.TranscriptDroppedBytes)
	}
}

func TestClaudeAdapterSkipsOversizedEventBeforeCapturedTerminalResult(t *testing.T) {
	stubClaudeCredentialsHome(t)
	workspace := t.TempDir()
	raw := []byte(strings.Join([]string{
		`{"type":"system","subtype":"init","model":"claude-sonnet-4-6"}`,
		`{"type":"assistant","message":{"model":"claude-sonnet-4-6","content":[{"type":"text","text":"` + strings.Repeat("x", int(maxClaudeStreamEventBytes)) + `"}]}}`,
		`{"type":"result","subtype":"success","result":"terminal output","total_cost_usd":0.25,"usage":{"input_tokens":120,"output_tokens":30}}`,
	}, "\n"))
	runner := &claudeSequenceRunner{
		rawOutputs: [][]byte{raw},
		acts: []func(ProcessRequest) error{
			func(req ProcessRequest) error {
				return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
			},
		},
	}
	adapter := &ClaudeAdapter{Command: []string{"claude"}, Runner: runner, KeychainReader: absentClaudeKeychain}

	out, err := adapter.Run(context.Background(), RunRequest{
		Envelope:           testEnvelope(workspace),
		Workspace:          workspace,
		CompletionPath:     DefaultResultPath,
		MaxTranscriptBytes: maxClaudeStreamEventBytes + 1024,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := out.Metrics[telemetry.AttrGenAIUsageInputTokens]; got != 120 {
		t.Fatalf("input token metric = %v, want 120", got)
	}
	if !bytes.Contains(out.Transcript, []byte("terminal output")) {
		t.Fatalf("transcript lost terminal output:\n%s", out.Transcript)
	}
	if !out.TranscriptTruncated || out.TranscriptDroppedBytes <= maxClaudeStreamEventBytes {
		t.Fatalf("truncation = %v, dropped = %d; want oversized event discarded", out.TranscriptTruncated, out.TranscriptDroppedBytes)
	}
}

func TestClaudeAdapterRecoveryPreservesTerminalResultBeyondTranscriptLimit(t *testing.T) {
	// limit must stay comfortably above the rendered prompt's size (a few KB,
	// and only grows as prompt.go's hints do) so the shared-rune-cap budget in
	// truncateTranscriptContents always has room to retain both prompts in
	// full; the oversized "y"-repeated assistant blob below (limit*2 bytes on
	// its own) is what this test actually means to force truncated.
	const limit = 8192
	stubClaudeCredentialsHome(t)
	workspace := t.TempDir()
	recovery := []byte(strings.Join([]string{
		`{"type":"system","subtype":"init","model":"claude-sonnet-4-6"}`,
		`{"type":"assistant","message":{"model":"claude-sonnet-4-6","content":[{"type":"text","text":"` + strings.Repeat("y", limit*2) + `"}]}}`,
		`{"type":"result","subtype":"success","result":"recovered terminal output","total_cost_usd":0.5,"usage":{"input_tokens":40,"output_tokens":10}}`,
	}, "\n"))
	runner := &claudeSequenceRunner{
		rawOutputs: [][]byte{[]byte(claudeResultStream), recovery},
		acts: []func(ProcessRequest) error{
			nil,
			func(req ProcessRequest) error {
				return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
			},
		},
	}
	adapter := &ClaudeAdapter{Command: []string{"claude"}, Runner: runner, KeychainReader: absentClaudeKeychain}

	out, err := adapter.Run(context.Background(), RunRequest{
		Envelope:           testEnvelope(workspace),
		Workspace:          workspace,
		CompletionPath:     DefaultResultPath,
		Timeout:            time.Minute,
		MaxTranscriptBytes: limit,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := out.Metrics[telemetry.AttrGenAIUsageInputTokens]; got != 160 {
		t.Fatalf("combined input token metric = %v, want 160", got)
	}
	if !out.TranscriptTruncated || out.TranscriptDroppedBytes <= 0 {
		t.Fatalf("truncation = %v, dropped = %d; want truncated recovery", out.TranscriptTruncated, out.TranscriptDroppedBytes)
	}
	if !bytes.Contains(out.Transcript, []byte("recovered terminal output")) {
		t.Fatalf("transcript lost recovery terminal output:\n%s", out.Transcript)
	}
	initialPrompt := string(runner.reqs[0].Stdin)
	recoveryPrompt := string(runner.reqs[1].Stdin)
	var orderedContent []string
	for _, line := range bytes.Split(bytes.TrimSpace(out.Transcript), []byte("\n")) {
		var event transcriptEvent
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatalf("decode canonical event: %v\n%s", err, line)
		}
		switch event.Content {
		case initialPrompt, "done", recoveryPrompt, "recovered terminal output":
			orderedContent = append(orderedContent, event.Content)
		}
	}
	want := []string{initialPrompt, "done", recoveryPrompt, "recovered terminal output"}
	if !slices.Equal(orderedContent, want) {
		t.Fatalf("prompt/result order = %q, want %q\n%s", orderedContent, want, out.Transcript)
	}
}

func TestClaudeAdapterRecoversMissingCompletionInSameSession(t *testing.T) {
	stubClaudeCredentialsHome(t)
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
			completion:     apiv1.ResultEnvelope{Status: apiv1.ResultSuccess},
		},
		{
			name:           "verdict",
			mode:           ModeReview,
			completionPath: DefaultVerdictPath,
			completion:     apiv1.Verdict{Decision: apiv1.VerdictPass},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workspace := t.TempDir()
			runner := &claudeSequenceRunner{
				results: []ProcessResult{
					{ExitCode: 0, Transcript: []byte(claudeResultStream)},
					{ExitCode: 0, Transcript: []byte(claudeResultStream)},
				},
				acts: []func(ProcessRequest) error{
					nil,
					func(req ProcessRequest) error {
						return WriteCompletion(req.Dir, tc.completionPath, tc.completion)
					},
				},
			}
			adapter := &ClaudeAdapter{Command: []string{"claude"}, Runner: runner, KeychainReader: absentClaudeKeychain}
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
			if len(runner.reqs) != 2 {
				t.Fatalf("process calls = %d, want 2", len(runner.reqs))
			}
			initialSession := commandOptionValue(runner.reqs[0].Command, "--session-id")
			recoverySession := commandOptionValue(runner.reqs[1].Command, "--resume")
			if initialSession == "" || recoverySession != initialSession {
				t.Fatalf("recovery did not resume initial session: initial=%q recovery=%q", initialSession, recoverySession)
			}
			if slices.Contains(runner.reqs[1].Command, "--session-id") {
				t.Fatalf("recovery started a new session: %v", runner.reqs[1].Command)
			}
			recoveryPrompt := string(runner.reqs[1].Stdin)
			if !strings.Contains(recoveryPrompt, tc.completionPath) ||
				!strings.Contains(recoveryPrompt, "previous turn ended without writing") {
				t.Fatalf("recovery prompt = %q", recoveryPrompt)
			}
			if len(out.Payload) == 0 {
				t.Fatal("completion payload is empty after recovery")
			}
			if got := out.Metrics[telemetry.AttrGenAIUsageInputTokens]; got != 120 {
				t.Fatalf("repeated cumulative input token metric = %v, want 120", got)
			}
		})
	}
}

func TestClaudeAdapterRepairsSchemaInvalidCompletionInSameSession(t *testing.T) {
	stubClaudeCredentialsHome(t)
	for _, invalid := range []string{
		`{"status":"success","outputs":{"findingResponses":{"finding":1}}}`,
		`{"status":"success","outputs":{"findingResponses":[{"finding":1}]}}`,
	} {
		t.Run(invalid, func(t *testing.T) {
			workspace := t.TempDir()
			sideEffects := 0
			runner := &claudeSequenceRunner{
				results: []ProcessResult{
					{ExitCode: 0, Transcript: []byte(claudeResultStream)},
					{ExitCode: 0, Transcript: []byte(claudeResultStream)},
				},
				acts: []func(ProcessRequest) error{
					func(req ProcessRequest) error {
						sideEffects++
						return writeRaw(req.Dir, DefaultResultPath, invalid)
					},
					func(req ProcessRequest) error {
						return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{
							Status:  apiv1.ResultSuccess,
							Outputs: map[string]any{"findingResponses": "[]"},
						})
					},
				},
			}
			adapter := &ClaudeAdapter{Command: []string{"claude"}, Runner: runner, KeychainReader: absentClaudeKeychain}
			out, err := adapter.Run(context.Background(), RunRequest{
				Mode:           ModeInvoke,
				Envelope:       testEnvelope(workspace),
				Workspace:      workspace,
				CompletionPath: DefaultResultPath,
				Timeout:        time.Minute,
				ValidateCompletion: func(payload []byte) error {
					var completion struct {
						Outputs map[string]any `json:"outputs"`
					}
					if err := json.Unmarshal(payload, &completion); err != nil {
						return err
					}
					if _, ok := completion.Outputs["findingResponses"].(string); !ok {
						return errors.New(`jsonschema: '/outputs/findingResponses' expected string, but got structured value`)
					}
					return nil
				},
			})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if sideEffects != 1 {
				t.Fatalf("completed side effects = %d, want 1", sideEffects)
			}
			if len(runner.reqs) != 2 {
				t.Fatalf("process calls = %d, want one bounded repair turn", len(runner.reqs))
			}
			recoveryPrompt := string(runner.reqs[1].Stdin)
			if !strings.Contains(recoveryPrompt, "/outputs/findingResponses") ||
				!strings.Contains(recoveryPrompt, "expected string") {
				t.Fatalf("repair prompt omitted validation detail: %q", recoveryPrompt)
			}
			if !strings.Contains(string(out.Payload), `"findingResponses":"[]"`) {
				t.Fatalf("completion payload = %s, want corrected scalar output", out.Payload)
			}
		})
	}
}

func TestClaudeAdapterFailsClearlyAfterInvalidCompletionRepairIsExhausted(t *testing.T) {
	stubClaudeCredentialsHome(t)
	workspace := t.TempDir()
	runner := &claudeSequenceRunner{
		results: []ProcessResult{
			{ExitCode: 0, Transcript: []byte(claudeResultStream)},
			{ExitCode: 0, Transcript: []byte(claudeResultStream)},
		},
		acts: []func(ProcessRequest) error{
			func(req ProcessRequest) error {
				return writeRaw(req.Dir, DefaultResultPath, `{"status":"success","outputs":{"bad":[]}}`)
			},
			func(req ProcessRequest) error {
				return writeRaw(req.Dir, DefaultResultPath, `{"status":"success","outputs":{"bad":[]}}`)
			},
		},
	}
	adapter := &ClaudeAdapter{Command: []string{"claude"}, Runner: runner, KeychainReader: absentClaudeKeychain}
	out, err := adapter.Run(context.Background(), RunRequest{
		Mode:           ModeInvoke,
		Envelope:       testEnvelope(workspace),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Timeout:        time.Minute,
		ValidateCompletion: func([]byte) error {
			return errors.New(`jsonschema: '/outputs/bad' expected scalar, but got array`)
		},
	})
	if !errors.Is(err, ErrInvalidCompletion) ||
		!strings.Contains(err.Error(), "/outputs/bad") ||
		!strings.Contains(err.Error(), "expected scalar") {
		t.Fatalf("Run error = %v, want schema path and validation message", err)
	}
	if len(runner.reqs) != 2 {
		t.Fatalf("process calls = %d, want exactly two", len(runner.reqs))
	}
	if len(out.Payload) == 0 {
		t.Fatal("exhausted invalid payload was not preserved for diagnostics")
	}
}

func commandOptionValue(command []string, option string) string {
	index := slices.Index(command, option)
	if index < 0 || index+1 >= len(command) {
		return ""
	}
	return command[index+1]
}

func TestClaudeAdapterFailsClosedWhenRecoveryOmitsCompletion(t *testing.T) {
	stubClaudeCredentialsHome(t)
	workspace := t.TempDir()
	runner := &claudeSequenceRunner{
		results: []ProcessResult{
			{ExitCode: 0, Transcript: []byte(claudeResultStream)},
			{ExitCode: 0, Transcript: []byte(claudeResultStream)},
		},
	}
	adapter := &ClaudeAdapter{Command: []string{"claude"}, Runner: runner, KeychainReader: absentClaudeKeychain}
	_, err := adapter.Run(context.Background(), RunRequest{
		Envelope:       testEnvelope(workspace),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Timeout:        time.Minute,
	})
	if !errors.Is(err, ErrNoCompletion) {
		t.Fatalf("Run error = %v, want ErrNoCompletion", err)
	}
	if len(runner.reqs) != 2 {
		t.Fatalf("process calls = %d, want 2", len(runner.reqs))
	}
}

func TestClaudeAdapterPreflightRequiresBinaryAndAuthentication(t *testing.T) {
	missing := &ClaudeAdapter{Command: []string{"definitely-not-a-real-claude-code-binary"}}
	if _, err := missing.Preflight(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "not found on PATH") {
		t.Fatalf("missing binary error = %v", err)
	}

	signedOutRunner := &claudeSequenceRunner{results: []ProcessResult{
		{ExitCode: 0, Transcript: []byte("2.1.214 (Claude Code)\n")},
		{ExitCode: 1, Transcript: []byte(`{"loggedIn":false}`)},
	}}
	signedOut := &ClaudeAdapter{Command: []string{"echo"}, Runner: signedOutRunner}
	if _, err := signedOut.Preflight(context.Background()); err == nil ||
		!strings.Contains(err.Error(), `{"loggedIn":false}`) ||
		!strings.Contains(err.Error(), "if this is an authentication failure, run `claude auth login`") {
		t.Fatalf("signed-out error = %v", err)
	}
	if len(signedOutRunner.reqs) != 2 ||
		!slices.Equal(signedOutRunner.reqs[1].Command, []string{"echo", "auth", "status"}) {
		t.Fatalf("auth probe = %v", signedOutRunner.reqs)
	}

	signedInRunner := &claudeSequenceRunner{results: []ProcessResult{
		{ExitCode: 0, Transcript: []byte("2.1.214 (Claude Code)\n")},
		{ExitCode: 0, Transcript: []byte(`{"loggedIn":true}`)},
	}}
	signedIn := &ClaudeAdapter{Command: []string{"echo"}, Runner: signedInRunner}
	info, err := signedIn.Preflight(context.Background())
	if err != nil {
		t.Fatalf("signed-in Preflight: %v", err)
	}
	if info.Version != "2.1.214 (Claude Code)" {
		t.Fatalf("version = %q", info.Version)
	}
}

func TestClaudeAdapterRejectsInvalidConfiguration(t *testing.T) {
	adapter := &ClaudeAdapter{}
	if err := adapter.ValidateConfig(" claude-sonnet-4-6", nil); err == nil {
		t.Fatal("expected whitespace-padded model to fail")
	}
	if err := adapter.ValidateConfig("", testHarnessOptions(t, map[string]interface{}{"temperature": 0.2})); err == nil {
		t.Fatal("expected unknown option to fail")
	}
	if err := adapter.ValidateConfig("", testHarnessOptions(t, map[string]interface{}{"effort": "extreme"})); err == nil {
		t.Fatal("expected invalid effort to fail")
	}
}

func TestClaudeAdapterValidateConfigRejectsUnsupportedModel(t *testing.T) {
	adapter := &ClaudeAdapter{}
	err := adapter.ValidateConfig("claude-nonexistent-model", nil)
	if err == nil {
		t.Fatal("expected unsupported model to fail admission")
	}
	if !strings.Contains(err.Error(), "claude-nonexistent-model") {
		t.Fatalf("error does not name the rejected model: %v", err)
	}
	if !strings.Contains(err.Error(), "claude-sonnet-5") {
		t.Fatalf("error does not list a valid model: %v", err)
	}
}

func TestClaudeAdapterValidateConfigAcceptsKnownModels(t *testing.T) {
	adapter := &ClaudeAdapter{}
	if err := adapter.ValidateConfig("", nil); err != nil {
		t.Fatalf("empty model should defer to the harness default: %v", err)
	}
	if err := adapter.ValidateConfig("auto", nil); err != nil {
		t.Fatalf(`"auto" should defer to the harness default: %v`, err)
	}
	for model := range claudeKnownModels {
		if err := adapter.ValidateConfig(model, nil); err != nil {
			t.Fatalf("ValidateConfig(%q): %v", model, err)
		}
	}
}

func TestClaudeAdapterConfinesSandboxRuntime(t *testing.T) {
	workspace := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	credentialDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(credentialDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(credentialDir, ".credentials.json"), []byte(`{"oauthToken":"stored-login"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	sandbox := &stubSandbox{}
	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0, Transcript: []byte(claudeResultStream)},
		act: func(req ProcessRequest) error {
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	adapter := &ClaudeAdapter{Command: []string{"claude-sandbox-test"}, Runner: runner, KeychainReader: absentClaudeKeychain}
	if _, err := adapter.Run(context.Background(), RunRequest{
		Envelope:       testEnvelope(workspace),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Sandbox:        sandbox,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(runner.lastReq.Command) < 3 || runner.lastReq.Command[0] != "sandbox-stub" ||
		runner.lastReq.Command[2] != "claude-sandbox-test" {
		t.Fatalf("command is not sandbox-wrapped: %v", runner.lastReq.Command)
	}
	for _, name := range []string{"CLAUDE_CONFIG_DIR=", "TMPDIR="} {
		found := false
		for _, entry := range runner.lastReq.Env {
			found = found || strings.HasPrefix(entry, name)
		}
		if !found {
			t.Fatalf("environment missing sandbox override %s: %v", name, runner.lastReq.Env)
		}
	}
	copied, err := os.ReadFile(filepath.Join(workspace, ".goobers", "sandbox", "claude-config", ".credentials.json"))
	if err != nil {
		t.Fatalf("read sandbox credential copy: %v", err)
	}
	if string(copied) != `{"oauthToken":"stored-login"}` {
		t.Fatalf("sandbox credential copy = %q", copied)
	}
}

func TestSeedClaudeCredentialsReadsMacOSKeychainWhenFileMissing(t *testing.T) {
	destination := t.TempDir()
	var gotService string
	err := seedClaudeCredentialsForPlatform(
		context.Background(),
		[]string{"HOME=" + t.TempDir()},
		destination,
		"darwin",
		func(_ context.Context, service string) ([]byte, error) {
			gotService = service
			return []byte("  {\"claudeAiOauth\":\"keychain-login\"}\n"), nil
		},
	)
	if err != nil {
		t.Fatalf("seedClaudeCredentialsForPlatform: %v", err)
	}
	if gotService != claudeCodeKeychainService {
		t.Fatalf("Keychain service = %q, want %q", gotService, claudeCodeKeychainService)
	}
	target := filepath.Join(destination, ".credentials.json")
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read seeded credentials: %v", err)
	}
	if want := `{"claudeAiOauth":"keychain-login"}`; string(got) != want {
		t.Fatalf("seeded credentials = %q, want %q", got, want)
	}
	// The seeded file holds an OAuth credential, so it must be provably
	// private on every platform. secfile.VerifyPrivate checks the DACL on
	// Windows (where mode bits are synthesized and meaningless) and the mode
	// on Unix (#2795).
	if err := secfile.VerifyPrivate(target); err != nil {
		t.Fatalf("seeded credentials are not private: %v", err)
	}
	// Unix additionally keeps its historical exact 0600 mode.
	if runtime.GOOS != "windows" {
		info, err := os.Stat(target)
		if err != nil {
			t.Fatalf("stat seeded credentials: %v", err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("seeded credential mode = %o, want 600", got)
		}
	}
}

func TestSeedClaudeCredentialsKeychainFailuresFailClosed(t *testing.T) {
	keychainErr := errors.New("item not found")
	err := seedClaudeCredentialsForPlatform(
		context.Background(),
		nil,
		t.TempDir(),
		"darwin",
		func(context.Context, string) ([]byte, error) {
			return nil, keychainErr
		},
	)
	if !errors.Is(err, keychainErr) || !strings.Contains(err.Error(), claudeCodeKeychainService) {
		t.Fatalf("keychain read error = %v, want wrapped error naming service", err)
	}

	err = seedClaudeCredentialsForPlatform(
		context.Background(),
		nil,
		t.TempDir(),
		"darwin",
		func(context.Context, string) ([]byte, error) {
			return []byte(" \n"), nil
		},
	)
	if err == nil || !strings.Contains(err.Error(), "empty Claude Code credentials") {
		t.Fatalf("empty keychain error = %v", err)
	}
}

// A Mac that never signed in to Claude Code has no keychain item, and
// security(1) reports that with exit status 44. That is the same "there is no
// stored login here" state the file branch treats as optional, so it must not
// fail the run — before this, the adapter was unusable on any such machine,
// which is every macOS CI runner (TestClaudeAdapterRunWiresGoobersIO failed
// there with `exit status 44` while passing on developer laptops that happened
// to have a real login stored). readOperatorClaudeKeychain reports that status
// as ErrClaudeKeychainItemNotFound; TestKeychainItemNotFoundMatchesOnlyExit44
// pins the classification without running security(1).
func TestSeedClaudeCredentialsKeychainItemNotFoundIsOptional(t *testing.T) {
	destination := t.TempDir()
	err := seedClaudeCredentialsForPlatform(
		context.Background(),
		[]string{"HOME=" + t.TempDir()},
		destination,
		"darwin",
		func(context.Context, string) ([]byte, error) {
			return nil, fmt.Errorf("%w: exit status 44", ErrClaudeKeychainItemNotFound)
		},
	)
	if err != nil {
		t.Fatalf("seedClaudeCredentialsForPlatform: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destination, ".credentials.json")); !os.IsNotExist(err) {
		t.Fatalf("stat seeded credentials = %v, want not-exist (nothing to seed)", err)
	}
}

func TestKeychainItemNotFoundMatchesOnlyExit44(t *testing.T) {
	notFound := exec.Command("sh", "-c", "exit 44").Run()
	if !isKeychainItemNotFound(notFound) {
		t.Fatalf("isKeychainItemNotFound(%v) = false, want true for exit status %d", notFound, keychainItemNotFoundExit)
	}
	// 51 is what a locked or sandboxed keychain returned in the #7447
	// reports: the credential state is unknown, so it must stay a failure.
	locked := exec.Command("sh", "-c", "exit 51").Run()
	if isKeychainItemNotFound(locked) {
		t.Fatalf("isKeychainItemNotFound(%v) = true, want false", locked)
	}
	if isKeychainItemNotFound(errors.New("exit status 44")) {
		t.Fatal("isKeychainItemNotFound matched a message instead of an exit status")
	}
}

// TestClaudeAdapterRefusesRealKeychainUnderTest pins the #7447 guard: a unit
// test that reaches credential seeding without injecting a KeychainReader
// panics instead of running security(1) against the developer's login
// Keychain. The guard fires on every platform so Linux CI catches the
// omission too. Only the reader selection is exercised here, so even a broken
// guard returns the real reader without calling it.
func TestClaudeAdapterRefusesRealKeychainUnderTest(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("nil KeychainReader under go test did not panic")
		}
	}()
	_ = (&ClaudeAdapter{}).keychainReader()
}

// TestReadOperatorClaudeKeychainRefusesUnderTest pins the backstop inside the
// real reader: called directly under go test it panics before security(1)
// runs, so naming the real reader in a unit test cannot reach the developer's
// Keychain either.
func TestReadOperatorClaudeKeychainRefusesUnderTest(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("real Keychain reader under go test did not panic")
		}
	}()
	_, _ = readOperatorClaudeKeychain(context.Background(), claudeCodeKeychainService)
}

func TestClaudeAdapterUsesInjectedKeychainReader(t *testing.T) {
	// An empty temp HOME: no credentials file, so on macOS seeding falls
	// through to the injected reader.
	t.Setenv("HOME", t.TempDir())
	workspace := t.TempDir()
	var gotService string
	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0, Transcript: []byte(claudeResultStream)},
		act: func(req ProcessRequest) error {
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	adapter := &ClaudeAdapter{
		Command: []string{"claude"},
		Runner:  runner,
		KeychainReader: func(_ context.Context, service string) ([]byte, error) {
			gotService = service
			return []byte(`{"claudeAiOauth":"fake-keychain-login"}`), nil
		},
	}
	if _, err := adapter.Run(context.Background(), RunRequest{
		Envelope:       testEnvelope(workspace),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if runtime.GOOS != "darwin" {
		if gotService != "" {
			t.Fatalf("Keychain reader consulted on %s", runtime.GOOS)
		}
		return
	}
	if gotService != claudeCodeKeychainService {
		t.Fatalf("Keychain service = %q, want %q", gotService, claudeCodeKeychainService)
	}
	configDir := ""
	for _, entry := range runner.lastReq.Env {
		if value, ok := strings.CutPrefix(entry, "CLAUDE_CONFIG_DIR="); ok {
			configDir = value
		}
	}
	seeded, err := os.ReadFile(filepath.Join(configDir, ".credentials.json"))
	if err != nil {
		t.Fatalf("read seeded credentials: %v", err)
	}
	if string(seeded) != `{"claudeAiOauth":"fake-keychain-login"}` {
		t.Fatalf("seeded credentials = %q, want the injected reader's login", seeded)
	}
}

// TestSeedClaudeCredentialsRefusesHostConfigDirUnderTest pins the file-side
// #7447 guard: the config dirs captured when the test binary started (the
// developer's real ~/.claude) are recognized, and a temp HOME is not. It
// checks the predicate, never a seeding call, so a broken guard cannot read
// the real file.
func TestSeedClaudeCredentialsRefusesHostConfigDirUnderTest(t *testing.T) {
	if len(hostClaudeConfigDirs) == 0 {
		t.Skip("test binary started with no HOME, USERPROFILE or CLAUDE_CONFIG_DIR; nothing to guard")
	}
	for _, dir := range hostClaudeConfigDirs {
		if !isHostClaudeConfigDir(dir + string(filepath.Separator)) {
			t.Fatalf("host config dir %s not recognized", dir)
		}
	}
	if isHostClaudeConfigDir(filepath.Join(t.TempDir(), ".claude")) {
		t.Fatal("temp HOME's .claude treated as the host config dir")
	}
	defer func() {
		if recover() == nil {
			t.Fatal("seeding from the host config dir under go test did not panic")
		}
	}()
	refuseHostClaudeConfigDirUnderTest(hostClaudeConfigDirs[0])
}

func TestSeedClaudeCredentialsMissingFileRemainsOptionalOutsideMacOS(t *testing.T) {
	called := false
	err := seedClaudeCredentialsForPlatform(
		context.Background(),
		[]string{"HOME=" + t.TempDir()},
		t.TempDir(),
		"linux",
		func(context.Context, string) ([]byte, error) {
			called = true
			return nil, errors.New("unexpected keychain read")
		},
	)
	if err != nil {
		t.Fatalf("seedClaudeCredentialsForPlatform: %v", err)
	}
	if called {
		t.Fatal("Keychain reader called outside macOS")
	}
}

// TestClaudeAdapterIsolatesAmbientConfigWithoutSandbox plants a fake ambient
// ~/.claude — settings.json with a hook, a plugin directory, and an MCP
// server config — and asserts an unsandboxed run neither points
// CLAUDE_CONFIG_DIR at it nor copies any of it into the isolated runtime
// directory the run actually uses; only stored credentials cross over.
func TestClaudeAdapterIsolatesAmbientConfigWithoutSandbox(t *testing.T) {
	workspace := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	ambientDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(filepath.Join(ambientDir, "plugins", "evil-plugin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ambientDir, "settings.json"),
		[]byte(`{"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"echo ambient-hook-fired"}]}]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ambientDir, "plugins", "evil-plugin", "plugin.json"),
		[]byte(`{"name":"evil-plugin"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ambientDir, ".mcp.json"),
		[]byte(`{"mcpServers":{"ambient":{"command":"ambient-mcp"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ambientDir, ".credentials.json"),
		[]byte(`{"oauthToken":"stored-login"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0, Transcript: []byte(claudeResultStream)},
		act: func(req ProcessRequest) error {
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	adapter := &ClaudeAdapter{Command: []string{"claude"}, Runner: runner, KeychainReader: absentClaudeKeychain}
	if _, err := adapter.Run(context.Background(), RunRequest{
		Envelope:       testEnvelope(workspace),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		HarnessVersion: "2.1.302 (Claude Code)",
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var configDir string
	for _, entry := range runner.lastReq.Env {
		if v, ok := strings.CutPrefix(entry, "CLAUDE_CONFIG_DIR="); ok {
			configDir = v
		}
	}
	if configDir == "" {
		t.Fatal("expected CLAUDE_CONFIG_DIR override on an unsandboxed run")
	}
	if configDir == ambientDir {
		t.Fatalf("CLAUDE_CONFIG_DIR points at the ambient config directory: %s", configDir)
	}
	for _, name := range []string{"plugins", ".mcp.json"} {
		if _, err := os.Stat(filepath.Join(configDir, name)); !os.IsNotExist(err) {
			t.Fatalf("ambient %s is visible in the isolated config dir: %v", name, err)
		}
	}
	settings, err := os.ReadFile(filepath.Join(configDir, "settings.json"))
	if err != nil || strings.Contains(string(settings), "ambient-hook-fired") || !strings.Contains(string(settings), "__claude-host-safety") {
		t.Fatalf("isolated settings must contain only the managed safety hook: %v", err)
	}
	copied, err := os.ReadFile(filepath.Join(configDir, ".credentials.json"))
	if err != nil {
		t.Fatalf("read isolated credential copy: %v", err)
	}
	if string(copied) != `{"oauthToken":"stored-login"}` {
		t.Fatalf("isolated credential copy = %q", copied)
	}
}

func TestClaudeAdapterRejectsSymlinkedSandboxRuntime(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, ".goobers"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace, ".goobers", "sandbox")); err != nil {
		t.Fatal(err)
	}
	adapter := &ClaudeAdapter{
		KeychainReader: absentClaudeKeychain,
		Command:        []string{"claude"},
		Runner:         &fakeProcessRunner{result: ProcessResult{ExitCode: 0}},
	}
	_, err := adapter.Run(context.Background(), RunRequest{
		Envelope:       testEnvelope(workspace),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Sandbox:        &stubSandbox{},
	})
	if err == nil || !strings.Contains(err.Error(), "not a plain directory") {
		t.Fatalf("Run error = %v, want symlink rejection", err)
	}
	if _, err := os.Stat(filepath.Join(outside, ".credentials.json")); !os.IsNotExist(err) {
		t.Fatalf("credential material escaped through symlink: %v", err)
	}
}

func runClaudeAdapterForCommand(t *testing.T, tools []string) []string {
	t.Helper()
	stubClaudeCredentialsHome(t)
	workspace := t.TempDir()
	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0},
		act: func(req ProcessRequest) error {
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	adapter := &ClaudeAdapter{Command: []string{"claude"}, Runner: runner, KeychainReader: absentClaudeKeychain}
	_, err := adapter.Run(context.Background(), RunRequest{
		Envelope:       testEnvelope(workspace),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Tools:          tools,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return runner.lastReq.Command
}

// TestClaudeAdapterEmptyToolAllowlistPreservesCommand pins #1471 acceptance
// criterion 4: an absent or empty tools declaration must produce byte-for-byte
// the same command as before tool enforcement existed.
func TestClaudeAdapterEmptyToolAllowlistPreservesCommand(t *testing.T) {
	for _, tc := range []struct {
		name  string
		tools []string
	}{
		{name: "nil tools", tools: nil},
		{name: "explicit empty tools", tools: []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			command := runClaudeAdapterForCommand(t, tc.tools)
			for _, want := range []string{"--permission-mode", "bypassPermissions"} {
				if !slices.Contains(command, want) {
					t.Errorf("command missing %q: %v", want, command)
				}
			}
			for _, unwanted := range []string{"--tools", "--allowedTools"} {
				if slices.Contains(command, unwanted) {
					t.Errorf("command unexpectedly carries %q: %v", unwanted, command)
				}
			}
		})
	}
}

// TestClaudeAdapterToolAllowlist pins #1471 acceptance criteria 1-3: a
// non-empty tools declaration constrains claude-code via --tools/
// --allowedTools instead of the unconditional bypassPermissions escape hatch,
// and a tool omitted from the declared list is unreachable (negative
// control).
func TestClaudeAdapterToolAllowlist(t *testing.T) {
	tests := []struct {
		name         string
		tools        []string
		wantIncluded []string
		wantOmitted  []string
	}{
		{
			name:         "concrete tools",
			tools:        []string{"Read", "Grep"},
			wantIncluded: []string{"Read", "Grep"},
			wantOmitted:  []string{"Bash", "Write", "Edit"},
		},
		{
			name:         "shell group expands",
			tools:        []string{"shell"},
			wantIncluded: []string{"Bash", "Read", "Edit", "Write", "Glob", "Grep"},
			wantOmitted:  []string{"WebFetch", "WebSearch"},
		},
		{
			name:         "telemetry group expands and excludes shell-only tools",
			tools:        []string{"telemetry"},
			wantIncluded: []string{"Read", "Glob", "Grep"},
			wantOmitted:  []string{"Bash", "Write", "Edit"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			command := runClaudeAdapterForCommand(t, tc.tools)
			if slices.Contains(command, "bypassPermissions") {
				t.Errorf("tool-constrained command still carries bypassPermissions: %v", command)
			}
			for _, flag := range []string{"--tools", "--allowedTools"} {
				idx := slices.Index(command, flag)
				if idx == -1 || idx+1 >= len(command) {
					t.Fatalf("command missing %q value: %v", flag, command)
				}
				value := command[idx+1]
				got := strings.Split(value, ",")
				for _, want := range tc.wantIncluded {
					if !slices.Contains(got, want) {
						t.Errorf("%s=%q missing %q", flag, value, want)
					}
				}
				for _, unwanted := range tc.wantOmitted {
					if slices.Contains(got, unwanted) {
						t.Errorf("%s=%q unexpectedly includes %q", flag, value, unwanted)
					}
				}
			}
		})
	}
}

func TestClaudeAdapterToolAllowlistRejectsCommaDelimitedEntry(t *testing.T) {
	workspace := t.TempDir()
	adapter := &ClaudeAdapter{
		KeychainReader: absentClaudeKeychain,
		Command:        []string{"claude"},
		Runner:         &fakeProcessRunner{result: ProcessResult{ExitCode: 0}},
	}
	_, err := adapter.Run(context.Background(), RunRequest{
		Envelope:       testEnvelope(workspace),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Tools:          []string{"Read,Write"},
	})
	if err == nil || !strings.Contains(err.Error(), "must not contain a comma") {
		t.Fatalf("Run error = %v, want comma rejection", err)
	}
}

func TestClaudeAdapterRepairAccountsForResumedSessionTotals(t *testing.T) {
	stubClaudeCredentialsHome(t)
	workspace := t.TempDir()
	runner := &claudeSequenceRunner{
		rawOutputs: [][]byte{readTestData(t, "claude-usage-initial.jsonl"), readTestData(t, "claude-usage-resumed.jsonl")},
		acts: []func(ProcessRequest) error{nil, func(req ProcessRequest) error {
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		}},
	}
	adapter := &ClaudeAdapter{Command: []string{"claude"}, Runner: runner, KeychainReader: absentClaudeKeychain}
	out, err := adapter.Run(context.Background(), RunRequest{
		Envelope: testEnvelope(workspace), Workspace: workspace, CompletionPath: DefaultResultPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.reqs) != 2 || commandOptionValue(runner.reqs[1].Command, "--resume") == "" {
		t.Fatal("expected same-session repair")
	}
	if out.Metrics[telemetry.AttrGenAIUsageInputTokens] != 150 || out.Metrics[telemetry.AttrUsageCostUSD] != 0.75 {
		t.Fatalf("resumed totals double counted: %v", out.Metrics)
	}
}
