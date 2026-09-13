package harness

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/mcpconfig"
	"github.com/goobers/goobers/internal/telemetry"
)

const codexCompletedStream = `{"type":"thread.started","thread_id":"thread-1"}
{"type":"turn.completed","usage":{"input_tokens":120,"cached_input_tokens":20,"output_tokens":30,"reasoning_output_tokens":5}}
`

type codexSequenceRunner struct {
	results  []ProcessResult
	requests []ProcessRequest
}

func (r *codexSequenceRunner) Run(_ context.Context, req ProcessRequest) (ProcessResult, error) {
	r.requests = append(r.requests, req)
	result := r.results[len(r.requests)-1]
	if req.StdoutCapture != nil {
		if _, err := req.StdoutCapture.Write(result.Transcript); err != nil {
			return result, err
		}
	}
	if len(r.requests) == len(r.results) {
		if err := WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}); err != nil {
			return result, err
		}
	}
	return result, nil
}

func TestCodexAdapterRunUsesIsolatedHeadlessContract(t *testing.T) {
	workspace := t.TempDir()
	ambientHome := filepath.Join(t.TempDir(), "ambient-codex")
	t.Setenv("CODEX_HOME", ambientHome)
	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0, Transcript: []byte(codexCompletedStream)},
		act: func(req ProcessRequest) error {
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{
				Status:  apiv1.ResultSuccess,
				Summary: "implemented",
			})
		},
	}
	adapter := &CodexAdapter{
		Command: []string{"codex"},
		Runner:  runner,
		EnvCapabilities: map[string]string{
			"agent:model": codexModelEnv,
		},
	}
	credentials := pushCredentials(t, "agent:model", "sk-test-codex")
	out, err := adapter.Run(context.Background(), RunRequest{
		Envelope:       testEnvelope(workspace, "agent:model"),
		Model:          "gpt-5-codex",
		HarnessOptions: testHarnessOptions(t, map[string]interface{}{"effort": "high"}),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Credentials:    credentials,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, want := range []string{
		"codex", "exec", "--json", "--ephemeral", "--ignore-rules", "--disable", "hooks",
		"--sandbox", "workspace-write", "--skip-git-repo-check", "-C", workspace,
		`projects.` + tomlQuote(filepath.Clean(workspace)) + `.trust_level="untrusted"`,
		"--model", "gpt-5-codex", `model_reasoning_effort="high"`, "-",
	} {
		if !slices.Contains(runner.lastReq.Command, want) {
			t.Errorf("command missing %q: %v", want, runner.lastReq.Command)
		}
	}
	if !strings.Contains(string(runner.lastReq.Stdin), "write your result as JSON") {
		t.Fatalf("stdin does not carry completion prompt: %q", runner.lastReq.Stdin)
	}
	if !slices.Contains(runner.lastReq.Env, codexModelEnv+"=sk-test-codex") {
		t.Fatalf("model credential missing from environment: %v", runner.lastReq.Env)
	}
	var isolatedHome string
	for _, entry := range runner.lastReq.Env {
		if name, value, ok := strings.Cut(entry, "="); ok && name == "CODEX_HOME" {
			isolatedHome = value
		}
	}
	if isolatedHome == "" || isolatedHome == ambientHome {
		t.Fatalf("CODEX_HOME = %q, want private path distinct from %q", isolatedHome, ambientHome)
	}
	runtimeRoot := filepath.Dir(isolatedHome)
	if strings.HasPrefix(runtimeRoot, workspace) {
		t.Fatalf("runtime root %q must be outside workspace %q", runtimeRoot, workspace)
	}
	for _, name := range []string{"TMPDIR", "TEMP", "TMP"} {
		found := false
		for _, entry := range runner.lastReq.Env {
			if envName, value, ok := strings.Cut(entry, "="); ok && envName == name {
				found = strings.HasPrefix(value, runtimeRoot)
			}
		}
		if !found {
			t.Errorf("%s was not redirected into the private runtime root: %v", name, runner.lastReq.Env)
		}
	}
	if _, err := os.Stat(runtimeRoot); !os.IsNotExist(err) {
		t.Fatalf("runtime root was not reclaimed: %v", err)
	}
	if got := out.Metrics[telemetry.AttrGenAIUsageInputTokens]; got != 120 {
		t.Fatalf("input tokens = %v, want 120", got)
	}
	if got := out.Metrics[telemetry.AttrGenAIUsageOutputTokens]; got != 30 {
		t.Fatalf("output tokens = %v, want 30", got)
	}
	if len(out.Payload) == 0 {
		t.Fatal("completion payload is empty")
	}
	assertAdapterLifecycle(t, out, "codex")
}

func TestCodexAdapterRejectsRestrictiveTools(t *testing.T) {
	workspace := t.TempDir()
	adapter := &CodexAdapter{Command: []string{"codex"}, Runner: &fakeProcessRunner{}}
	_, err := adapter.Run(context.Background(), RunRequest{
		Envelope:       testEnvelope(workspace),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Tools:          []string{"telemetry"},
	})
	if err == nil || !strings.Contains(err.Error(), "restrictive tools declarations are unsupported") {
		t.Fatalf("Run error = %v", err)
	}
}

func TestCodexAdapterMaterializesScopedMCPConfig(t *testing.T) {
	workspace := t.TempDir()
	t.Setenv("SAFE_TOOLCHAIN_VAR", "available")
	t.Setenv("LANG", "codex-test-locale")
	var capturedConfig string
	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0, Transcript: []byte(codexCompletedStream)},
		act: func(req ProcessRequest) error {
			for _, entry := range req.Env {
				if name, value, ok := strings.Cut(entry, "="); ok && name == "CODEX_HOME" {
					data, err := os.ReadFile(filepath.Join(value, "config.toml"))
					if err != nil {
						return err
					}
					capturedConfig = string(data)
				}
			}
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	adapter := &CodexAdapter{
		Command: []string{"codex"},
		Runner:  runner,
		ExtraEnvAllowlist: []string{
			"SAFE_TOOLCHAIN_VAR",
		},
		EnvCapabilities: map[string]string{
			"agent:model":   codexModelEnv,
			"contents:read": "TEST_CONTEXT_TOKEN",
		},
	}
	creds := mcpTestCredentials(t, "agent:model", "sk-test-codex", "contents:read", "context-secret")
	_, err := adapter.Run(context.Background(), RunRequest{
		Envelope:       testEnvelope(workspace, "agent:model", "contents:read"),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Credentials:    creds,
		MCPServers: []apiv1.MCPServer{{
			Name:    "context",
			Command: "context-server",
			Args:    []string{"--stdio"},
			CredentialRefs: []apiv1.MCPCredentialRef{{
				Capability: "contents:read",
				Env:        "CONTEXT_TOKEN",
			}},
		}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !slices.Contains(runner.lastReq.Env, "CONTEXT_TOKEN=context-secret") {
		t.Fatalf("MCP credential missing from environment: %v", runner.lastReq.Env)
	}
	config := capturedConfig
	for _, want := range []string{
		`[mcp_servers."context"]`,
		`command = "context-server"`,
		`args = ["--stdio"]`,
		`env_vars = ["CONTEXT_TOKEN"]`,
		`required = true`,
	} {
		if !strings.Contains(config, want) {
			t.Errorf("config missing %q:\n%s", want, config)
		}
	}
	if strings.Contains(config, "context-secret") {
		t.Fatalf("credential leaked into config:\n%s", config)
	}
	command := strings.Join(runner.lastReq.Command, "\n")
	if !strings.Contains(command, `shell_environment_policy.set=`) ||
		!strings.Contains(command, `shell_environment_policy.inherit="none"`) ||
		!strings.Contains(command, `"LANG" = "codex-test-locale"`) {
		t.Fatalf("shell environment policy does not explicitly set a non-inheriting environment:\n%s", command)
	}
	for _, excludedName := range []string{"SAFE_TOOLCHAIN_VAR", "TEST_CONTEXT_TOKEN", "CONTEXT_TOKEN", "CODEX_API_KEY"} {
		if strings.Contains(command, `"`+excludedName+`"`) {
			t.Fatalf("shell environment policy exposed non-core or credential variable %q:\n%s", excludedName, command)
		}
	}
	if err := mcpconfig.ValidateForHarness(apiv1.HarnessCodex, []apiv1.MCPServer{{
		Name:    "context",
		Command: "context-server",
		CredentialRefs: []apiv1.MCPCredentialRef{{
			Capability: "contents:read",
			Env:        "CONTEXT_TOKEN",
		}},
	}}, []string{"contents:read"}, nil); err != nil {
		t.Fatalf("Codex MCP validation: %v", err)
	}
}

func TestCodexAdapterRejectsReservedMCPEnvironment(t *testing.T) {
	for _, envName := range []string{
		codexModelEnv,
		"GOOBERS_MCP_CREDENTIAL_1_0",
		"HOME",
		"SSL_CERT_FILE",
		"OPENAI_IDENTITY_TOKEN_FILE",
	} {
		t.Run(envName, func(t *testing.T) {
			workspace := t.TempDir()
			adapter := &CodexAdapter{
				Command: []string{"codex"},
				Runner:  &fakeProcessRunner{},
				EnvCapabilities: map[string]string{
					"agent:model": codexModelEnv,
				},
			}
			credentials := mcpTestCredentials(t, "agent:model", "sk-test-codex", "contents:read", "context-secret")
			_, err := adapter.Run(context.Background(), RunRequest{
				Envelope:       testEnvelope(workspace, "agent:model", "contents:read"),
				Workspace:      workspace,
				CompletionPath: DefaultResultPath,
				Credentials:    credentials,
				MCPServers: []apiv1.MCPServer{{
					Name:    "context",
					Command: "context-server",
					CredentialRefs: []apiv1.MCPCredentialRef{{
						Capability: "contents:read",
						Env:        envName,
					}},
				}},
			})
			if err == nil || !strings.Contains(err.Error(), `environment variable "`+envName+`" is reserved`) {
				t.Fatalf("Run error = %v", err)
			}
		})
	}
}

func TestCodexAdapterRejectsDeclaredGoobersIOCollision(t *testing.T) {
	workspace := t.TempDir()
	adapter := &CodexAdapter{
		Command: []string{"codex"},
		Runner:  &fakeProcessRunner{},
		EnvCapabilities: map[string]string{
			"agent:model": codexModelEnv,
		},
	}
	_, err := adapter.Run(context.Background(), RunRequest{
		Envelope:            testEnvelope(workspace, "agent:model"),
		Workspace:           workspace,
		CompletionPath:      DefaultResultPath,
		Credentials:         pushCredentials(t, "agent:model", "sk-test-codex"),
		GoobersIORegistered: true,
		MCPServers: []apiv1.MCPServer{{
			Name:    goobersIOServerName,
			Command: "other-server",
		}},
	})
	if err == nil || !strings.Contains(err.Error(), `server name "goobers-io" is reserved`) {
		t.Fatalf("Run error = %v", err)
	}
}

func TestCodexAdapterIncludesRecoveryUsage(t *testing.T) {
	workspace := t.TempDir()
	runner := &codexSequenceRunner{results: []ProcessResult{
		{ExitCode: 0, Transcript: []byte(`{"type":"turn.completed","usage":{"input_tokens":90,"output_tokens":5}}` + "\n")},
		{ExitCode: 0, Transcript: []byte(`{"type":"turn.completed","usage":{"input_tokens":30,"output_tokens":7}}` + "\n")},
	}}
	adapter := &CodexAdapter{
		Command: []string{"codex"},
		Runner:  runner,
		EnvCapabilities: map[string]string{
			"agent:model": codexModelEnv,
		},
	}
	out, err := adapter.Run(context.Background(), RunRequest{
		Envelope:       testEnvelope(workspace, "agent:model"),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Credentials:    pushCredentials(t, "agent:model", "sk-test-codex"),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := out.Metrics[telemetry.AttrGenAIUsageInputTokens]; got != 120 {
		t.Fatalf("input tokens = %v, want 120", got)
	}
	if got := out.Metrics[telemetry.AttrGenAIUsageOutputTokens]; got != 12 {
		t.Fatalf("output tokens = %v, want 12", got)
	}
}

func TestParseCodexJSONLRejectsMissingTerminalEvent(t *testing.T) {
	parsed, err := parseCodexJSONL([]byte(`{"type":"turn.started"}` + "\n"))
	if err != nil {
		t.Fatalf("parseCodexJSONL: %v", err)
	}
	if parsed.completed || parsed.failed != "" {
		t.Fatalf("parsed = %#v", parsed)
	}
}

func TestCodexJSONLCaptureBoundsOneEventWithoutBoundingTheStream(t *testing.T) {
	capture := newCodexJSONLCapture(128)
	for i := 0; i < 100; i++ {
		if _, err := capture.Write([]byte(`{"type":"turn.started"}` + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := capture.Write([]byte(`{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":2}}` + "\n")); err != nil {
		t.Fatal(err)
	}
	parsed, err := capture.result()
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if !parsed.completed || parsed.metrics[telemetry.AttrGenAIUsageInputTokens] != 1 {
		t.Fatalf("parsed = %#v", parsed)
	}

	oversized := newCodexJSONLCapture(32)
	_, _ = oversized.Write([]byte(`{"type":"item.completed","item":{"text":"this event is deliberately too large"}}`))
	if _, err := oversized.result(); err == nil || !strings.Contains(err.Error(), "exceeded 32 bytes") {
		t.Fatalf("oversized result error = %v", err)
	}
}

func TestCodexAdapterRejectsStoredLogin(t *testing.T) {
	sourceHome := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourceHome, "auth.json"), []byte(`{"token":"stored"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", sourceHome)
	workspace := t.TempDir()
	runner := &fakeProcessRunner{}
	adapter := &CodexAdapter{Command: []string{"codex"}, Runner: runner}
	_, err := adapter.Run(context.Background(), RunRequest{
		Envelope:       testEnvelope(workspace),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
	})
	if err == nil || !strings.Contains(err.Error(), "OpenAI API key is required") {
		t.Fatalf("Run error = %v", err)
	}
}

func TestCodexAdapterRejectsSymlinkedPrompt(t *testing.T) {
	workspace := t.TempDir()
	goobersDir := filepath.Join(workspace, ".goobers")
	if err := os.Mkdir(goobersDir, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(goobersDir, "prompt.md")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	adapter := &CodexAdapter{Command: []string{"codex"}, Runner: &fakeProcessRunner{}}
	_, err := adapter.Run(context.Background(), RunRequest{
		Envelope:       testEnvelope(workspace),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
	})
	if err == nil || !strings.Contains(err.Error(), "prompt.md") || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("Run error = %v", err)
	}
	data, readErr := os.ReadFile(outside)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(data) != "original" {
		t.Fatalf("outside file changed: %q", data)
	}
}

func TestCodexAdapterPreflightUsesVersionAndAPIKey(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeProcessRunner{result: ProcessResult{ExitCode: 0, Transcript: []byte("codex-cli 1.2.3\n")}}
	adapter := &CodexAdapter{
		Command: []string{executable},
		Runner:  runner,
		ModelCredential: func(context.Context) (string, error) {
			return "sk-test-codex", nil
		},
	}
	info, err := adapter.Preflight(context.Background())
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if info.Version != "codex-cli 1.2.3" {
		t.Fatalf("version = %q", info.Version)
	}
	if got := strings.Join(runner.lastReq.Command, " "); !strings.Contains(got, "--version") {
		t.Fatalf("preflight command = %v", runner.lastReq.Command)
	}
}

func TestCodexAdapterPreflightUsesAmbientAPIKey(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(codexModelEnv, "sk-pod-codex")
	runner := &fakeProcessRunner{result: ProcessResult{ExitCode: 0, Transcript: []byte("codex-cli 1.2.3\n")}}
	adapter := &CodexAdapter{Command: []string{executable}, Runner: runner}
	if _, err := adapter.Preflight(context.Background()); err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if !slices.Contains(runner.lastReq.Env, codexModelEnv+"=sk-pod-codex") {
		t.Fatalf("ambient API key missing from preflight environment: %v", runner.lastReq.Env)
	}
}

func TestCodexAdapterPreflightPrefersConfiguredCredential(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(codexModelEnv, "sk-ambient-codex")
	configuredErr := errors.New("configured credential unavailable")
	adapter := &CodexAdapter{
		Command: []string{executable},
		Runner:  &fakeProcessRunner{},
		ModelCredential: func(context.Context) (string, error) {
			return "", configuredErr
		},
	}
	if _, err := adapter.Preflight(context.Background()); !errors.Is(err, configuredErr) {
		t.Fatalf("Preflight error = %v, want configured credential error", err)
	}
}
