package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
)

func environmentValue(env []string, name string) (string, bool) {
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, name+"="); ok {
			return value, true
		}
	}
	return "", false
}

func TestPrepareCopilotMCPMaterializesScopedConfig(t *testing.T) {
	workspace := t.TempDir()
	req := RunRequest{
		Envelope:  testEnvelope(workspace, "contents:read", "github:issues:write"),
		Workspace: workspace,
		Credentials: twoTokenCredentials(
			t,
			"contents:read", "local-mcp-secret",
			"github:issues:write", "remote-mcp-secret",
		),
		MCPServers: []apiv1.MCPServer{
			{
				Name:    "local-context",
				Command: "context-server",
				Args:    []string{"--stdio"},
				CredentialRefs: []apiv1.MCPCredentialRef{{
					Capability: "contents:read",
					Env:        "CONTEXT_TOKEN",
				}},
			},
			{
				Name: "remote-context",
				URL:  "https://mcp.example.test/api",
				CredentialRefs: []apiv1.MCPCredentialRef{{
					Capability: "github:issues:write",
					Header:     "Authorization",
					Scheme:     apiv1.MCPHeaderSchemeBearer,
				}},
			},
		},
	}

	env, err := prepareCopilotMCP(
		context.Background(),
		req,
		[]string{"HOME=/ambient/operator", "COPILOT_HOME=/ambient/copilot"},
		"/ambient/copilot",
	)
	if err != nil {
		t.Fatal(err)
	}
	home, ok := environmentValue(env, "COPILOT_HOME")
	runtimeRoot := filepath.Join(workspace, filepath.FromSlash(copilotMCPRuntimeSubdir))
	if !ok || !strings.HasPrefix(home, runtimeRoot+string(filepath.Separator)) {
		t.Fatalf("COPILOT_HOME = %q, want invocation-local path under %q", home, runtimeRoot)
	}
	configPath := filepath.Join(home, "mcp-config.json")
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("local-mcp-secret")) || bytes.Contains(raw, []byte("remote-mcp-secret")) {
		t.Fatalf("credential leaked into MCP config: %s", raw)
	}
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("MCP config mode = %o, want 600", info.Mode().Perm())
	}

	var config copilotMCPConfig
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	local := config.MCPServers["local-context"]
	if local.Type != "local" || local.Command != "context-server" ||
		!slices.Equal(local.Args, []string{"--stdio"}) ||
		local.Env["CONTEXT_TOKEN"] != "${GOOBERS_MCP_CREDENTIAL_0_0}" {
		t.Fatalf("local server config = %#v", local)
	}
	remote := config.MCPServers["remote-context"]
	if remote.Type != "http" || remote.URL != "https://mcp.example.test/api" ||
		remote.Headers["Authorization"] != "Bearer ${GOOBERS_MCP_CREDENTIAL_1_0}" {
		t.Fatalf("remote server config = %#v", remote)
	}
	for _, want := range []string{
		"GOOBERS_MCP_CREDENTIAL_0_0=local-mcp-secret",
		"GOOBERS_MCP_CREDENTIAL_1_0=remote-mcp-secret",
	} {
		if !slices.Contains(env, want) {
			t.Fatalf("scoped environment missing %q: %v", want, env)
		}
	}
}

func TestCopilotAdapterMCPUsesStoredAuthenticationWithoutModelGrant(t *testing.T) {
	ambientHome := t.TempDir()
	t.Setenv("HOME", ambientHome)
	copilotHome := filepath.Join(ambientHome, ".copilot")
	if err := os.MkdirAll(copilotHome, 0o700); err != nil {
		t.Fatal(err)
	}
	storedConfig := []byte(`{"oauthToken":"stored-auth","model":"stored-model"}`)
	if err := os.WriteFile(filepath.Join(copilotHome, "config.json"), storedConfig, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(copilotHome, "mcp-config.json"),
		[]byte(`{"mcpServers":{"ambient":{"type":"local","command":"ambient-server","tools":["*"]}}}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0},
		act: func(req ProcessRequest) error {
			scopedHome, ok := environmentValue(req.Env, "COPILOT_HOME")
			if !ok || scopedHome == copilotHome {
				return fmt.Errorf("COPILOT_HOME = %q, %v; want isolated home", scopedHome, ok)
			}
			gotConfig, err := os.ReadFile(filepath.Join(scopedHome, "config.json"))
			if err != nil {
				return err
			}
			if !bytes.Equal(gotConfig, storedConfig) {
				return fmt.Errorf("stored config = %s, want %s", gotConfig, storedConfig)
			}
			if _, ok := environmentValue(req.Env, "COPILOT_MODEL_TOKEN"); ok {
				return fmt.Errorf("agent:model token was injected without a grant")
			}
			rawMCP, err := os.ReadFile(filepath.Join(scopedHome, "mcp-config.json"))
			if err != nil {
				return err
			}
			var config copilotMCPConfig
			if err := json.Unmarshal(rawMCP, &config); err != nil {
				return err
			}
			if len(config.MCPServers) != 1 || config.MCPServers["declared"].Command != "declared-server" {
				return fmt.Errorf("scoped MCP servers = %#v, want only declared server", config.MCPServers)
			}
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	adapter := &CopilotAdapter{
		Command:         []string{"copilot"},
		Runner:          runner,
		EnvCapabilities: map[string]string{"agent:model": "COPILOT_MODEL_TOKEN"},
	}
	workspace := t.TempDir()
	_, err := adapter.Run(context.Background(), RunRequest{
		Envelope:       testEnvelope(workspace),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		MCPServers:     []apiv1.MCPServer{{Name: "declared", Command: "declared-server"}},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCopilotAdapterScopesMCPServersToOneInvocation(t *testing.T) {
	ambientHome := t.TempDir()
	t.Setenv("HOME", ambientHome)
	t.Setenv("COPILOT_HOME", filepath.Join(ambientHome, "ambient-copilot"))
	t.Setenv(copilotWorkspaceMCPEnv, "1")
	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0},
		act: func(req ProcessRequest) error {
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	adapter := &CopilotAdapter{
		Command:           []string{"copilot"},
		Runner:            runner,
		ExtraEnvAllowlist: []string{"COPILOT_HOME", copilotWorkspaceMCPEnv},
	}
	workspace := t.TempDir()
	req := RunRequest{
		Envelope:       testEnvelope(workspace),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		MCPServers:     []apiv1.MCPServer{{Name: "local-context", Command: "context-server"}},
	}
	if _, err := adapter.Run(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(runner.lastReq.Command, "--disable-builtin-mcps") {
		t.Fatalf("scoped invocation did not disable ambient built-in MCPs: %v", runner.lastReq.Command)
	}
	scopedHome, ok := environmentValue(runner.lastReq.Env, "COPILOT_HOME")
	if !ok || scopedHome == filepath.Join(ambientHome, "ambient-copilot") {
		t.Fatalf("scoped invocation did not isolate Copilot home: %v", runner.lastReq.Env)
	}
	if _, ok := environmentValue(runner.lastReq.Env, copilotWorkspaceMCPEnv); ok {
		t.Fatalf("scoped invocation inherited workspace MCP discovery: %v", runner.lastReq.Env)
	}

	workspace = t.TempDir()
	req = RunRequest{
		Envelope:       testEnvelope(workspace),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
	}
	if _, err := adapter.Run(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(runner.lastReq.Command, "--disable-builtin-mcps") {
		t.Fatalf("MCP-free invocation command changed: %v", runner.lastReq.Command)
	}
	if home, ok := environmentValue(runner.lastReq.Env, "COPILOT_HOME"); !ok || home != filepath.Join(ambientHome, "ambient-copilot") {
		t.Fatalf("MCP-free invocation changed ambient behavior: %v", runner.lastReq.Env)
	}
	if value, ok := environmentValue(runner.lastReq.Env, copilotWorkspaceMCPEnv); !ok || value != "1" {
		t.Fatalf("MCP-free invocation changed workspace MCP discovery behavior: %v", runner.lastReq.Env)
	}
}

func TestMCPCredentialIsScrubbedFromJournalAndTelemetry(t *testing.T) {
	const secret = "MCP-CANARY-opaque-value-732"
	registry, scrubber := journal.DefaultScrubber()
	if got := journal.NewPatternScrubber().Scrub([]byte(secret)); string(got) != secret {
		t.Fatalf("test canary is pattern-visible: %q", got)
	}
	workspace := t.TempDir()
	runner := &fakeProcessRunner{
		result: ProcessResult{
			ExitCode:   0,
			Transcript: []byte("remote MCP failed with Authorization: Bearer " + secret),
		},
		act: func(req ProcessRequest) error {
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	recorder := &fakeRecorder{}
	executor, err := NewExecutor(
		&CopilotAdapter{Command: []string{"copilot"}, Runner: runner},
		testInjector(t, "REPO_TOKEN_ENV", secret, registry),
		recorder,
		recorder,
		recorder,
		scrubber,
		"",
		WithMCPServers([]apiv1.MCPServer{{
			Name: "remote-context",
			URL:  "https://mcp.example.test/api",
			CredentialRefs: []apiv1.MCPCredentialRef{{
				Capability: "repo:read",
				Header:     "Authorization",
				Scheme:     apiv1.MCPHeaderSchemeBearer,
			}},
		}}),
	)
	if err != nil {
		t.Fatal(err)
	}

	exporter := telemetry.NewMemoryExporter()
	client, err := telemetry.New(context.Background(), telemetry.Config{
		ServiceName:  "mcp-secret-test",
		SpanExporter: exporter,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })
	ctx, span, err := client.StartTask(context.Background(), telemetry.TaskAttributes{
		Gaggle: "example", WorkflowID: "default-implement", RunID: "0af7651916cd43dd8448eb211c80319c",
		TaskID: "implement", TaskType: telemetry.StageTypeAgentic,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := executor.Invoke(ctx, testEnvelope(workspace, "repo:read")); err != nil {
		t.Fatal(err)
	}
	span.Succeed("done")
	if err := client.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	if len(recorder.spans) != 1 || bytes.Contains(recorder.spans[0].data, []byte(secret)) {
		t.Fatalf("MCP credential leaked into journal-bound span: %#v", recorder.spans)
	}
	for _, recorded := range exporter.Spans() {
		for _, attr := range recorded.Attributes() {
			if strings.Contains(attr.Value.Emit(), secret) {
				t.Fatalf("MCP credential leaked into telemetry attribute %q", attr.Key)
			}
		}
		for _, event := range recorded.Events() {
			for _, attr := range event.Attributes {
				if strings.Contains(attr.Value.Emit(), secret) {
					t.Fatalf("MCP credential leaked into telemetry event attribute %q", attr.Key)
				}
			}
		}
	}
	home, ok := environmentValue(runner.lastReq.Env, "COPILOT_HOME")
	if !ok {
		t.Fatal("scoped Copilot home missing")
	}
	config, err := os.ReadFile(filepath.Join(home, "mcp-config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(config, []byte(secret)) {
		t.Fatalf("MCP credential leaked into scoped config: %s", config)
	}
}
