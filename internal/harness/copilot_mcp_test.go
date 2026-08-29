package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/mcpconfig"
	"github.com/goobers/goobers/internal/telemetry"
	telemetrytest "github.com/goobers/goobers/test/testsupport/telemetry"

	"go.opentelemetry.io/otel/attribute"
)

func mcpTestInjector(t *testing.T, registrar credentials.SecretRegistrar, capabilityTokens ...string) *credentials.Injector {
	t.Helper()
	if len(capabilityTokens)%2 != 0 {
		t.Fatal("mcpTestInjector requires capability/token pairs")
	}
	refs := make([]credentials.TokenRef, 0, len(capabilityTokens)/2)
	grants := make([]credentials.Grant, 0, len(capabilityTokens)/2)
	for i := 0; i < len(capabilityTokens); i += 2 {
		envName := fmt.Sprintf("GOOBERS_TEST_MCP_TOKEN_%d", i/2)
		refName := fmt.Sprintf("mcp-token-%d", i/2)
		t.Setenv(envName, capabilityTokens[i+1])
		refs = append(refs, credentials.TokenRef{Name: refName, Env: envName})
		grants = append(grants, credentials.Grant{Goober: "test", Capability: capabilityTokens[i], Ref: refName})
	}
	resolver, err := credentials.NewResolver(refs)
	if err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(capabilityTokens)/2)
	for i := 0; i < len(capabilityTokens); i += 2 {
		keys = append(keys, capabilityTokens[i])
	}
	injector, err := credentials.NewGooberInjectorWithCredentialKeys(resolver, "test", grants, keys, registrar)
	if err != nil {
		t.Fatal(err)
	}
	return injector
}

func mcpTestCredentials(t *testing.T, capabilityTokens ...string) *credentials.Set {
	t.Helper()
	injector := mcpTestInjector(t, noopRegistrar{}, capabilityTokens...)
	capabilities := make([]string, 0, len(capabilityTokens)/2)
	for i := 0; i < len(capabilityTokens); i += 2 {
		capabilities = append(capabilities, capabilityTokens[i])
	}
	set, err := injector.Materialize(context.Background(), capabilities)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func environmentValue(env []string, name string) (string, bool) {
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, name+"="); ok {
			return value, true
		}
	}
	return "", false
}

func TestPrepareCopilotMCPMaterializesOnlyDeclaredTools(t *testing.T) {
	workspace := t.TempDir()
	req := RunRequest{
		Envelope:  testEnvelope(workspace, "contents:read", "github:issues:write"),
		Workspace: workspace,
		Tools:     []string{"reachability"},
		Credentials: twoTokenCredentials(
			t,
			"contents:read", "local-mcp-secret",
			mcpconfig.BYOCredentialKey("vendor-api"), "remote-mcp-secret",
		),
		MCPServers: []apiv1.MCPServer{
			{
				Name:    "local-context",
				Command: "context-server",
				Args:    []string{"--stdio"},
				CredentialRefs: []apiv1.MCPCredentialRef{
					{
						Capability: "contents:read",
						Env:        "CONTEXT_TOKEN",
					},
					{
						Kind: apiv1.MCPCredentialKindBYO,
						Ref:  "vendor-api",
						Env:  "VENDOR_TOKEN",
					},
				},
			},
			{
				Name: "remote-context",
				URL:  "https://mcp.example.test/api",
				CredentialRefs: []apiv1.MCPCredentialRef{{
					Kind:   apiv1.MCPCredentialKindBYO,
					Ref:    "vendor-api",
					Header: "Authorization",
					Scheme: apiv1.MCPHeaderSchemeBearer,
				}},
			},
		},
	}

	env, err := prepareCopilotMCP(
		context.Background(),
		req,
		[]string{"HOME=/ambient/operator", "COPILOT_HOME=/ambient/copilot"},
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
	// Windows has no unix-style owner/group/other permission bits: the write
	// path below still asks for 0o600 (correct — that's the POSIX-meaningful
	// request), but os.Stat on Windows always reports a plain writable file
	// back regardless. Asserting an exact 0600 there would be asserting a
	// permission model Windows doesn't have, not checking real behavior.
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("MCP config mode = %o, want 600", info.Mode().Perm())
	}

	var config copilotMCPConfig
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	local := config.MCPServers["local-context"]
	if local.Type != "local" || local.Command != "context-server" ||
		!slices.Equal(local.Args, []string{"--stdio"}) ||
		!slices.Equal(local.Tools, []string{"reachability"}) ||
		local.Env["CONTEXT_TOKEN"] != "${GOOBERS_MCP_CREDENTIAL_0_0}" ||
		local.Env["VENDOR_TOKEN"] != "${GOOBERS_MCP_CREDENTIAL_0_1}" {
		t.Fatalf("local server config = %#v", local)
	}
	remote := config.MCPServers["remote-context"]
	if remote.Type != "http" || remote.URL != "https://mcp.example.test/api" ||
		!slices.Equal(remote.Tools, []string{"reachability"}) ||
		remote.Headers["Authorization"] != "Bearer ${GOOBERS_MCP_CREDENTIAL_1_0}" {
		t.Fatalf("remote server config = %#v", remote)
	}
	if slices.Contains(local.Tools, "*") || slices.Contains(local.Tools, "omitted") {
		t.Fatalf("local server exposed an undeclared tool: %v", local.Tools)
	}
	for _, want := range []string{
		"GOOBERS_MCP_CREDENTIAL_0_0=local-mcp-secret",
		"GOOBERS_MCP_CREDENTIAL_0_1=remote-mcp-secret",
		"GOOBERS_MCP_CREDENTIAL_1_0=remote-mcp-secret",
	} {
		if !slices.Contains(env, want) {
			t.Fatalf("scoped environment missing %q: %v", want, env)
		}
	}
}

func TestPrepareCopilotMCPRejectsCredentialExposureToLocalSibling(t *testing.T) {
	workspace := t.TempDir()
	_, err := prepareCopilotMCP(context.Background(), RunRequest{
		Envelope:    testEnvelope(workspace),
		Workspace:   workspace,
		Credentials: mcpTestCredentials(t, mcpconfig.BYOCredentialKey("vendor-api"), "remote-mcp-secret"),
		MCPServers: []apiv1.MCPServer{
			{Name: "local-context", Command: "context-server"},
			{
				Name: "vendor-context",
				URL:  "https://vendor.example.test/mcp",
				CredentialRefs: []apiv1.MCPCredentialRef{{
					Kind:   apiv1.MCPCredentialKindBYO,
					Ref:    "vendor-api",
					Header: "Authorization",
				}},
			},
		},
	}, nil)
	if err == nil ||
		!strings.Contains(err.Error(), `local stdio server "local-context" cannot isolate credential "mcp:vendor-api"`) {
		t.Fatalf("prepareCopilotMCP error = %v, want local credential-isolation rejection", err)
	}
	if _, statErr := os.Stat(filepath.Join(workspace, filepath.FromSlash(copilotMCPRuntimeSubdir))); !os.IsNotExist(statErr) {
		t.Fatalf("unsafe MCP configuration reached materialization: %v", statErr)
	}
}

func TestPrepareCopilotMCPLeavesOmittedToolUnreachable(t *testing.T) {
	workspace := t.TempDir()
	env, err := prepareCopilotMCP(context.Background(), RunRequest{
		Envelope:   testEnvelope(workspace),
		Workspace:  workspace,
		MCPServers: []apiv1.MCPServer{{Name: "context", Command: "context-server"}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	home, ok := environmentValue(env, "COPILOT_HOME")
	if !ok {
		t.Fatal("scoped Copilot home missing")
	}
	raw, err := os.ReadFile(filepath.Join(home, "mcp-config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var config copilotMCPConfig
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	tools := config.MCPServers["context"].Tools
	if tools == nil || len(tools) != 0 {
		t.Fatalf("omitted tool is reachable through materialized policy: %v", tools)
	}
}

func TestPrepareCopilotMCPRejectsWildcardToolBeforeMaterialization(t *testing.T) {
	workspace := t.TempDir()
	_, err := prepareCopilotMCP(context.Background(), RunRequest{
		Envelope:   testEnvelope(workspace),
		Workspace:  workspace,
		MCPServers: []apiv1.MCPServer{{Name: "context", Command: "context-server"}},
		Tools:      []string{"*"},
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "must be an explicit tool name") {
		t.Fatalf("prepareCopilotMCP error = %v, want wildcard rejection", err)
	}
	if _, statErr := os.Stat(filepath.Join(workspace, filepath.FromSlash(copilotMCPRuntimeSubdir))); !os.IsNotExist(statErr) {
		t.Fatalf("wildcard MCP policy reached materialization: %v", statErr)
	}
}

// TestPrepareCopilotMCPRefusesToTraverseAWorkspaceSymlink covers #2413 for
// the scoped MCP runtime root: it is created in the harness's own process,
// before the spawned copilot subprocess is sandboxed, under a workspace that
// may hold repository-controlled content. A symlink planted at .goobers must
// not redirect the runtime root — and with it the scoped COPILOT_HOME and
// its mcp-config.json — outside the workspace.
func TestPrepareCopilotMCPRefusesToTraverseAWorkspaceSymlink(t *testing.T) {
	workspace := t.TempDir()
	outsideDir := t.TempDir()
	if err := os.Symlink(outsideDir, filepath.Join(workspace, ".goobers")); err != nil {
		t.Fatal(err)
	}

	_, err := prepareCopilotMCP(context.Background(), RunRequest{
		Envelope:   testEnvelope(workspace),
		Workspace:  workspace,
		MCPServers: []apiv1.MCPServer{{Name: "context", Command: "context-server"}},
	}, nil)
	if err == nil {
		t.Fatal("expected prepareCopilotMCP to refuse to traverse the symlinked .goobers directory")
	}
	if _, statErr := os.Lstat(filepath.Join(outsideDir, "mcp")); !os.IsNotExist(statErr) {
		t.Fatalf("MCP runtime root was created outside the workspace through the symlink: err=%v", statErr)
	}
}

// TestPrepareCopilotMCPKeepsTheWorkspaceSpelling pins that the scoped
// runtime root — and so the COPILOT_HOME and config path handed to the
// subprocess — stays under the workspace root the caller passed rather than
// the canonicalized root safepath resolves through. Where the workspace is
// reached through a symlink (macOS's /var, a Windows short path), a
// canonicalized prefix would name a location the caller's sandbox policy
// never described.
func TestPrepareCopilotMCPKeepsTheWorkspaceSpelling(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(t.TempDir(), workspace); err != nil {
		t.Fatal(err)
	}

	env, err := prepareCopilotMCP(context.Background(), RunRequest{
		Envelope:   testEnvelope(workspace),
		Workspace:  workspace,
		MCPServers: []apiv1.MCPServer{{Name: "context", Command: "context-server"}},
	}, nil)
	if err != nil {
		t.Fatalf("prepareCopilotMCP: %v", err)
	}
	runtimeRoot := filepath.Join(workspace, filepath.FromSlash(copilotMCPRuntimeSubdir))
	var home string
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, "COPILOT_HOME="); ok {
			home = value
		}
	}
	if !strings.HasPrefix(home, runtimeRoot+string(filepath.Separator)) {
		t.Fatalf("COPILOT_HOME = %q, want under %q", home, runtimeRoot)
	}
}

func TestCopilotAdapterMCPRequiresMaterializedModelCredential(t *testing.T) {
	invoked := false
	adapter := &CopilotAdapter{
		Command:                        []string{"copilot"},
		Runner:                         &fakeProcessRunner{act: func(ProcessRequest) error { invoked = true; return nil }},
		EnvCapabilities:                map[string]string{"agent:model": "COPILOT_MODEL_TOKEN"},
		OptionalCredentialCapabilities: map[string]bool{"agent:model": true},
	}
	workspace := t.TempDir()
	_, err := adapter.Run(context.Background(), RunRequest{
		Envelope:       testEnvelope(workspace, "agent:model"),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		MCPServers:     []apiv1.MCPServer{{Name: "declared", Command: "declared-server"}},
	})
	if err == nil || !strings.Contains(err.Error(), "external MCP servers require a materialized agent:model credential") {
		t.Fatalf("Run error = %v, want missing materialized agent:model credential", err)
	}
	if invoked {
		t.Fatal("Copilot process ran without materialized model authentication")
	}
}

func TestCopilotAdapterMCPUsesMaterializedModelAuthenticationWithoutCopyingAmbientAuthentication(t *testing.T) {
	const ambientAuthCanary = "AMBIENT-AUTH-CANARY-732"
	const modelToken = "SCOPED-MODEL-TOKEN-732"
	for _, profileSource := range []string{"USERPROFILE", "HOMEDRIVE/HOMEPATH"} {
		t.Run(profileSource, func(t *testing.T) {
			for _, name := range []string{"HOME", "COPILOT_HOME", "USERPROFILE", "HOMEDRIVE", "HOMEPATH"} {
				t.Setenv(name, "")
				if err := os.Unsetenv(name); err != nil {
					t.Fatal(err)
				}
			}

			profileHome := t.TempDir()
			switch profileSource {
			case "USERPROFILE":
				t.Setenv("USERPROFILE", profileHome)
			case "HOMEDRIVE/HOMEPATH":
				homeDrive := t.TempDir()
				homePath := string(filepath.Separator) + "operator"
				profileHome = homeDrive + homePath
				t.Setenv("HOMEDRIVE", homeDrive)
				t.Setenv("HOMEPATH", homePath)
			}
			copilotHome := filepath.Join(profileHome, ".copilot")
			if err := os.MkdirAll(copilotHome, 0o700); err != nil {
				t.Fatal(err)
			}
			storedConfig := []byte(`{"oauthToken":"` + ambientAuthCanary + `","provider":{"type":"byok","apiKey":"` + ambientAuthCanary + `"}}`)
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
					if err == nil {
						return fmt.Errorf("ambient Copilot config was copied into scoped home: %s", gotConfig)
					}
					if !os.IsNotExist(err) {
						return err
					}
					if token, ok := environmentValue(req.Env, "COPILOT_MODEL_TOKEN"); !ok || token != modelToken {
						return fmt.Errorf("materialized agent:model token = %q, %v", token, ok)
					}
					rawMCP, err := os.ReadFile(filepath.Join(scopedHome, "mcp-config.json"))
					if err != nil {
						return err
					}
					if bytes.Contains(rawMCP, []byte(ambientAuthCanary)) {
						return fmt.Errorf("ambient authentication canary leaked into scoped MCP config")
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
				Envelope:       testEnvelope(workspace, "agent:model"),
				Workspace:      workspace,
				CompletionPath: DefaultResultPath,
				MCPServers:     []apiv1.MCPServer{{Name: "declared", Command: "declared-server"}},
				Tools:          []string{"reachability"},
				Credentials:    mcpTestCredentials(t, "agent:model", modelToken),
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCopilotAdapterScopesMCPServersToOneInvocation(t *testing.T) {
	ambientHome := t.TempDir()
	workspaceMCPCaseVariant := strings.ToLower(copilotWorkspaceMCPEnv)
	t.Setenv("HOME", ambientHome)
	t.Setenv("COPILOT_HOME", filepath.Join(ambientHome, "ambient-copilot"))
	t.Setenv(workspaceMCPCaseVariant, "1")
	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0},
		act: func(req ProcessRequest) error {
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	adapter := &CopilotAdapter{
		Command:                        []string{"copilot"},
		Runner:                         runner,
		EnvCapabilities:                map[string]string{"agent:model": "COPILOT_MODEL_TOKEN"},
		OptionalCredentialCapabilities: map[string]bool{"agent:model": true},
		ExtraEnvAllowlist:              []string{"COPILOT_HOME", workspaceMCPCaseVariant},
	}
	workspace := t.TempDir()
	req := RunRequest{
		Envelope:       testEnvelope(workspace, "agent:model"),
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		MCPServers:     []apiv1.MCPServer{{Name: "local-context", Command: "context-server"}},
		Credentials:    mcpTestCredentials(t, "agent:model", "scoped-model-token"),
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
	if _, ok := environmentValue(runner.lastReq.Env, workspaceMCPCaseVariant); ok {
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
	if value, ok := environmentValue(runner.lastReq.Env, workspaceMCPCaseVariant); !ok || value != "1" {
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
		&CopilotAdapter{
			Command:         []string{"copilot"},
			Runner:          runner,
			EnvCapabilities: map[string]string{"agent:model": "COPILOT_MODEL_TOKEN"},
		},
		mcpTestInjector(t, registry,
			mcpconfig.BYOCredentialKey("sharepoint"), secret,
			"agent:model", "scoped-model-token",
		),
		recorder,
		recorder,
		recorder,
		scrubber,
		"",
		WithMCPServers([]apiv1.MCPServer{{
			Name: "remote-context",
			URL:  "https://mcp.example.test/api",
			CredentialRefs: []apiv1.MCPCredentialRef{{
				Kind:   apiv1.MCPCredentialKindBYO,
				Ref:    "sharepoint",
				Header: "Authorization",
				Scheme: apiv1.MCPHeaderSchemeBearer,
			}},
		}}),
	)
	if err != nil {
		t.Fatal(err)
	}

	exporter := telemetrytest.NewMemoryExporter()
	client, err := telemetry.New(context.Background(), telemetry.Config{
		ServiceName:  "mcp-secret-test",
		SpanExporter: exporter,
		Scrubber:     scrubber,
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
	if _, err := executor.Invoke(ctx, testEnvelope(workspace, "agent:model")); err != nil {
		t.Fatal(err)
	}
	span.Event("mcp.request", attribute.String("authorization", "Bearer "+secret))
	span.Succeed("done")
	if err := client.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	if len(recorder.spans) != 1 || bytes.Contains(recorder.spans[0].data, []byte(secret)) {
		t.Fatalf("MCP credential leaked into journal-bound span: %#v", recorder.spans)
	}
	sawScrubbedTelemetryCredential := false
	for _, recorded := range exporter.Spans() {
		for _, attr := range recorded.Attributes() {
			if strings.Contains(attr.Value.String(), secret) {
				t.Fatalf("MCP credential leaked into telemetry attribute %q", attr.Key)
			}
		}
		for _, event := range recorded.Events() {
			for _, attr := range event.Attributes {
				if strings.Contains(attr.Value.String(), secret) {
					t.Fatalf("MCP credential leaked into telemetry event attribute %q", attr.Key)
				}
				if attr.Key == "authorization" && strings.Contains(attr.Value.String(), journal.Redacted) {
					sawScrubbedTelemetryCredential = true
				}
			}
		}
	}
	if !sawScrubbedTelemetryCredential {
		t.Fatal("registry-backed MCP credential did not traverse the scrubbed telemetry path")
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
