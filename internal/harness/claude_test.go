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
	"github.com/goobers/goobers/internal/telemetry"
)

const claudeResultStream = `{"type":"system","subtype":"init","model":"claude-sonnet-4-6","session_id":"session-1"}
{"type":"assistant","message":{"model":"claude-sonnet-4-6","content":[{"type":"text","text":"done"}]}}
{"type":"result","subtype":"success","result":"done","total_cost_usd":0.25,"usage":{"input_tokens":120,"output_tokens":30},"modelUsage":{"claude-sonnet-4-6":{"inputTokens":120,"outputTokens":30,"costUSD":0.25}}}
`

func TestClaudeAdapterPreservesLifecycleEvents(t *testing.T) {
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
	adapter := &ClaudeAdapter{Command: []string{"claude-lifecycle-test"}, Runner: runner}
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
		Command: []string{"claude"},
		Runner:  runner,
		EnvCapabilities: map[string]string{
			"agent:model":         "ANTHROPIC_API_KEY",
			"github:issues:write": "GH_TOKEN",
		},
	}
	credentials := twoTokenCredentials(t,
		"agent:model", "anthropic-key",
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
		"claude", "-p", "--output-format", "stream-json", "--verbose",
		"--permission-mode", "bypassPermissions", "--model", "claude-sonnet-4-6",
		"--effort", "high",
	} {
		if !slices.Contains(command, want) {
			t.Errorf("command missing %q: %v", want, command)
		}
	}
	if len(command) < 2 || command[len(command)-2] != "--" {
		t.Fatalf("prompt is not the last argv element behind \"--\": %v", command)
	}
	prompt := commandPromptValue(command)
	if prompt == "" || !strings.Contains(prompt, "write your result as JSON") {
		t.Fatalf("command does not carry the completion-file prompt: %v", command)
	}
	if command[len(command)-1] != prompt {
		t.Fatalf("prompt is not the final argv element: %v", command)
	}
	for _, token := range []string{"anthropic-key", "github-token"} {
		for _, arg := range command {
			if strings.Contains(arg, token) {
				t.Fatalf("credential leaked into argv: %v", command)
			}
		}
	}
	for _, want := range []string{"ANTHROPIC_API_KEY=anthropic-key", "GH_TOKEN=github-token"} {
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

// TestBuildClaudeArgvPromptsLeadingWithDashesStayPositional pins the fix for
// #2090: every shipped instructions.md opens with YAML frontmatter ("---"),
// and claude's CLI parser scans all argv positions for option-shaped tokens,
// so a naive "-p <prompt> <flags...>" layout misparses the prompt as an
// unknown flag. The prompt must be the final argv element, behind "--".
func TestBuildClaudeArgvPromptsLeadingWithDashesStayPositional(t *testing.T) {
	for _, prompt := range []string{
		"---\nrole: curator\n---\n\ndo the thing",
		"-x single dash prefix",
		"--",
	} {
		t.Run(prompt, func(t *testing.T) {
			argv, promptArg, sessionSelectorArg := buildClaudeArgv(
				[]string{"claude"},
				[]string{"--output-format", "stream-json"},
				"claude-sonnet-4-6",
				"high",
				"session-1",
				prompt,
			)
			if got := argv[len(argv)-1]; got != prompt {
				t.Fatalf("prompt is not the final argv element: %v", argv)
			}
			if len(argv) < 2 || argv[len(argv)-2] != "--" {
				t.Fatalf("prompt is not preceded by \"--\": %v", argv)
			}
			if promptArg != len(argv)-1 {
				t.Fatalf("promptArg = %d, want %d (last index): %v", promptArg, len(argv)-1, argv)
			}
			if argv[promptArg] != prompt {
				t.Fatalf("argv[promptArg] = %q, want %q", argv[promptArg], prompt)
			}
			if argv[sessionSelectorArg] != "--session-id" {
				t.Fatalf("argv[sessionSelectorArg] = %q, want %q", argv[sessionSelectorArg], "--session-id")
			}
			for _, want := range []string{"--model", "claude-sonnet-4-6", "--effort", "high", "--session-id", "session-1"} {
				if !slices.Contains(argv, want) {
					t.Errorf("argv missing %q: %v", want, argv)
				}
			}
			// Simulate the completion-recovery path, which overwrites both
			// indices in place: promptArg with a fresh prompt, and the
			// "--session-id" token itself with "--resume" so the value
			// that follows it is reinterpreted as the resumed session id.
			recoveryArgv := append([]string(nil), argv...)
			recoveryArgv[promptArg] = "recovery prompt"
			recoveryArgv[sessionSelectorArg] = "--resume"
			if recoveryArgv[len(recoveryArgv)-1] != "recovery prompt" {
				t.Fatalf("recovery prompt did not land at the final argv element: %v", recoveryArgv)
			}
			if commandOptionValue(recoveryArgv, "--resume") != "session-1" {
				t.Fatalf("--resume did not carry the original session id: %v", recoveryArgv)
			}
		})
	}
}

// TestBuildClaudeArgvShiftUnderSandboxWrapping asserts promptArg and
// sessionSelectorArg remain correct once confineArgv prepends sandbox
// wrapper arguments ahead of the whole command, in both the sandboxed and
// non-sandboxed branches of Run.
func TestBuildClaudeArgvShiftUnderSandboxWrapping(t *testing.T) {
	prompt := "---\nrole: curator\n---\n\ndo the thing"
	argv, promptArg, sessionSelectorArg := buildClaudeArgv(
		[]string{"claude"}, nil, "", "", "session-1", prompt,
	)
	sandbox := &stubSandbox{}
	wrapped, shift, err := confineArgv(sandbox, argv, t.TempDir(), nil)
	if err != nil {
		t.Fatalf("confineArgv: %v", err)
	}
	promptArg += shift
	sessionSelectorArg += shift
	if wrapped[promptArg] != prompt {
		t.Fatalf("shifted promptArg = %d does not carry the prompt: %v", promptArg, wrapped)
	}
	if wrapped[sessionSelectorArg] != "--session-id" {
		t.Fatalf("shifted sessionSelectorArg = %d does not carry --session-id: %v", sessionSelectorArg, wrapped)
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

// stubClaudeCredentialsHome points HOME at a fresh directory seeded with a
// stored credential, so an unsandboxed Run's unconditional credential-seeding
// finds a file to copy instead of falling through to the real macOS
// Keychain — the fallback that TestSeedClaudeCredentialsReadsMacOSKeychainWhenFileMissing
// exercises deliberately, but that other tests must not hit incidentally.
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
	adapter := &ClaudeAdapter{Command: []string{"claude"}, Runner: runner}

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
	adapter := &ClaudeAdapter{Command: []string{"claude"}, Runner: runner}

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
	const limit = 4096
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
	adapter := &ClaudeAdapter{Command: []string{"claude"}, Runner: runner}

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
	initialPrompt := commandPromptValue(runner.reqs[0].Command)
	recoveryPrompt := commandPromptValue(runner.reqs[1].Command)
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
			adapter := &ClaudeAdapter{Command: []string{"claude"}, Runner: runner}
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
			recoveryPrompt := commandPromptValue(runner.reqs[1].Command)
			if !strings.Contains(recoveryPrompt, tc.completionPath) ||
				!strings.Contains(recoveryPrompt, "previous turn ended without writing") {
				t.Fatalf("recovery prompt = %q", recoveryPrompt)
			}
			if len(out.Payload) == 0 {
				t.Fatal("completion payload is empty after recovery")
			}
			if got := out.Metrics[telemetry.AttrGenAIUsageInputTokens]; got != 240 {
				t.Fatalf("combined input token metric = %v, want 240", got)
			}
		})
	}
}

func commandOptionValue(command []string, option string) string {
	index := slices.Index(command, option)
	if index < 0 || index+1 >= len(command) {
		return ""
	}
	return command[index+1]
}

// commandPromptValue returns the positional prompt argv carries behind its
// "--" terminator, so a prompt beginning with "-" is never mistaken for one
// of the option tokens above.
func commandPromptValue(command []string) string {
	index := slices.Index(command, "--")
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
	adapter := &ClaudeAdapter{Command: []string{"claude"}, Runner: runner}
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
	adapter := &ClaudeAdapter{Command: []string{"claude"}, Runner: runner}
	if _, err := adapter.Run(context.Background(), RunRequest{
		Envelope:       testEnvelope(workspace),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Sandbox:        sandbox,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(runner.lastReq.Command) < 3 || runner.lastReq.Command[0] != "sandbox-stub" ||
		runner.lastReq.Command[2] != "claude" {
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
	// Unix mode bits do not port to NTFS: os.WriteFile's mode argument and
	// os.Chmod only toggle the read-only attribute there, so the 0600 this
	// seeding requests surfaces back as 0666 (see internal/platform/secfile's
	// doc comment, which is why that package verifies privacy via the DACL
	// rather than Perm()). Asserting 0600 on Windows therefore just asserts a
	// fiction, so it is skipped here.
	//
	// Unlike the copilot MCP config file — which skips the same assertion
	// because it holds no credential worth protecting — this file DOES hold
	// one, and os.Chmod genuinely fails to protect it on Windows: the
	// credential ends up with whatever DACL it inherits from its parent
	// directory. Skipping the assertion keeps the gate honest about what the
	// platform can express; it does not make the file private. That real gap
	// is tracked in #2795.
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
// to have a real login stored).
func TestSeedClaudeCredentialsKeychainItemNotFoundIsOptional(t *testing.T) {
	notFound := exec.Command("sh", "-c", "exit 44").Run()
	var exitErr *exec.ExitError
	if !errors.As(notFound, &exitErr) || exitErr.ExitCode() != keychainItemNotFoundExit {
		t.Fatalf("fixture error = %v, want *exec.ExitError with status %d", notFound, keychainItemNotFoundExit)
	}
	destination := t.TempDir()
	err := seedClaudeCredentialsForPlatform(
		context.Background(),
		[]string{"HOME=" + t.TempDir()},
		destination,
		"darwin",
		func(context.Context, string) ([]byte, error) { return nil, notFound },
	)
	if err != nil {
		t.Fatalf("seedClaudeCredentialsForPlatform: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destination, ".credentials.json")); !os.IsNotExist(err) {
		t.Fatalf("stat seeded credentials = %v, want not-exist (nothing to seed)", err)
	}
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
	adapter := &ClaudeAdapter{Command: []string{"claude"}, Runner: runner}
	if _, err := adapter.Run(context.Background(), RunRequest{
		Envelope:       testEnvelope(workspace),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
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
	for _, name := range []string{"settings.json", "plugins", ".mcp.json"} {
		if _, err := os.Stat(filepath.Join(configDir, name)); !os.IsNotExist(err) {
			t.Fatalf("ambient %s is visible in the isolated config dir: %v", name, err)
		}
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
		Command: []string{"claude"},
		Runner:  &fakeProcessRunner{result: ProcessResult{ExitCode: 0}},
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
	adapter := &ClaudeAdapter{Command: []string{"claude"}, Runner: runner}
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
		Command: []string{"claude"},
		Runner:  &fakeProcessRunner{result: ProcessResult{ExitCode: 0}},
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
