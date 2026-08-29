package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/mcpconfig"
)

func TestPrepareClaudeMCPMaterializesDeclaredServers(t *testing.T) {
	workspace := t.TempDir()
	req := RunRequest{
		Envelope:  testEnvelope(workspace, "contents:read", "github:issues:write"),
		Workspace: workspace,
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
					{Capability: "contents:read", Env: "CONTEXT_TOKEN"},
					{Kind: apiv1.MCPCredentialKindBYO, Ref: "vendor-api", Env: "VENDOR_TOKEN"},
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

	configPath, envAdditions, err := prepareClaudeMCP(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	// resolveRooted canonicalizes the workspace root itself before joining
	// (a #2408 review finding, also true here) — on a platform where
	// t.TempDir() is reached through a symlink (macOS's /var -> /private/var),
	// compare against the same resolved form rather than the raw workspace.
	resolvedWorkspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	runtimeRoot := filepath.Join(resolvedWorkspace, filepath.FromSlash(claudeMCPRuntimeSubdir))
	if !strings.HasPrefix(configPath, runtimeRoot+string(filepath.Separator)) {
		t.Fatalf("config path = %q, want under %q", configPath, runtimeRoot)
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("local-mcp-secret")) || bytes.Contains(raw, []byte("remote-mcp-secret")) {
		t.Fatalf("credential leaked into MCP config: %s", raw)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(configPath)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("MCP config mode = %o, want 600", info.Mode().Perm())
		}
	}

	var config claudeMCPConfig
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatal(err)
	}
	local := config.MCPServers["local-context"]
	if local.Type != "stdio" || local.Command != "context-server" ||
		!slices.Equal(local.Args, []string{"--stdio"}) ||
		local.Env["CONTEXT_TOKEN"] != "${GOOBERS_MCP_CREDENTIAL_0_0}" ||
		local.Env["VENDOR_TOKEN"] != "${GOOBERS_MCP_CREDENTIAL_0_1}" {
		t.Fatalf("local server config = %#v", local)
	}
	remote := config.MCPServers["remote-context"]
	if remote.Type != "http" || remote.URL != "https://mcp.example.test/api" ||
		remote.Headers["Authorization"] != "Bearer ${GOOBERS_MCP_CREDENTIAL_1_0}" {
		t.Fatalf("remote server config = %#v", remote)
	}
	for _, want := range []string{
		"GOOBERS_MCP_CREDENTIAL_0_0=local-mcp-secret",
		"GOOBERS_MCP_CREDENTIAL_0_1=remote-mcp-secret",
		"GOOBERS_MCP_CREDENTIAL_1_0=remote-mcp-secret",
	} {
		if !slices.Contains(envAdditions, want) {
			t.Fatalf("env additions missing %q: %v", want, envAdditions)
		}
	}
}

// TestPrepareClaudeMCPRejectsCredentialExposureToLocalSibling is the
// claude-code counterpart of TestPrepareCopilotMCPRejectsCredentialExposureToLocalSibling
// (#1492 AC3): live-verified that claude-code's stdio MCP servers inherit
// the full parent process environment exactly like Copilot's, so the same
// isolation rejection is required here, not just on Copilot.
func TestPrepareClaudeMCPRejectsCredentialExposureToLocalSibling(t *testing.T) {
	workspace := t.TempDir()
	_, _, err := prepareClaudeMCP(context.Background(), RunRequest{
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
	})
	if err == nil ||
		!strings.Contains(err.Error(), `local stdio server "local-context" cannot isolate credential "mcp:vendor-api"`) ||
		!strings.Contains(err.Error(), "claude-code") {
		t.Fatalf("prepareClaudeMCP error = %v, want local credential-isolation rejection naming claude-code", err)
	}
	if _, statErr := os.Stat(filepath.Join(workspace, filepath.FromSlash(claudeMCPRuntimeSubdir))); !os.IsNotExist(statErr) {
		t.Fatalf("unsafe MCP configuration reached materialization: %v", statErr)
	}
}

func TestPrepareClaudeMCPRejectsWildcardToolBeforeMaterialization(t *testing.T) {
	workspace := t.TempDir()
	_, _, err := prepareClaudeMCP(context.Background(), RunRequest{
		Envelope:   testEnvelope(workspace),
		Workspace:  workspace,
		MCPServers: []apiv1.MCPServer{{Name: "context", Command: "context-server"}},
		Tools:      []string{"*"},
	})
	if err == nil || !strings.Contains(err.Error(), "must be an explicit tool name") {
		t.Fatalf("prepareClaudeMCP error = %v, want wildcard rejection", err)
	}
	if _, statErr := os.Stat(filepath.Join(workspace, filepath.FromSlash(claudeMCPRuntimeSubdir))); !os.IsNotExist(statErr) {
		t.Fatalf("wildcard MCP policy reached materialization: %v", statErr)
	}
}

func TestPrepareClaudeMCPEmptyWithoutDeclaredServers(t *testing.T) {
	workspace := t.TempDir()
	configPath, envAdditions, err := prepareClaudeMCP(context.Background(), RunRequest{
		Envelope:  testEnvelope(workspace),
		Workspace: workspace,
	})
	if err != nil {
		t.Fatal(err)
	}
	if configPath != "" || envAdditions != nil {
		t.Fatalf("expected no-op without declared servers, got configPath=%q envAdditions=%v", configPath, envAdditions)
	}
}

// TestPrepareClaudeMCPRefusesToTraverseAWorkspaceSymlink mirrors the
// Copilot-side and goobers-io-side #2408/#2413-class regression tests:
// this write happens in the harness's own trusted process, before the
// spawned claude subprocess is sandboxed, against a workspace that may
// contain repository-controlled content.
func TestPrepareClaudeMCPRefusesToTraverseAWorkspaceSymlink(t *testing.T) {
	workspace := t.TempDir()
	outsideDir := t.TempDir()
	if err := os.Symlink(outsideDir, filepath.Join(workspace, ".goobers")); err != nil {
		t.Fatal(err)
	}

	_, _, err := prepareClaudeMCP(context.Background(), RunRequest{
		Envelope:   testEnvelope(workspace),
		Workspace:  workspace,
		MCPServers: []apiv1.MCPServer{{Name: "context", Command: "context-server"}},
	})
	if err == nil {
		t.Fatal("expected prepareClaudeMCP to refuse to traverse the symlinked .goobers directory")
	}
	if _, statErr := os.Lstat(filepath.Join(outsideDir, "mcp")); !os.IsNotExist(statErr) {
		t.Fatalf("runtime directory was created outside the workspace through the symlink: err=%v", statErr)
	}
}

// TestClaudeAdapterRunCombinesDeclaredMCPServersWithGoobersIO is the
// Run()-level integration test: confirms a declared mcpServers registration
// and goobers-io's own registration coexist under one repeated --mcp-config
// flag invocation (confirmed live that claude-code merges repeated
// --mcp-config values), --strict-mcp-config appears exactly once, and the
// resolved credential lands in the subprocess environment.
func TestClaudeAdapterRunCombinesDeclaredMCPServersWithGoobersIO(t *testing.T) {
	stubClaudeCredentialsHome(t)
	workspace := t.TempDir()
	runner := &fakeProcessRunner{
		result: ProcessResult{ExitCode: 0},
		act: func(req ProcessRequest) error {
			return WriteCompletion(req.Dir, DefaultResultPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
		},
	}
	adapter := &ClaudeAdapter{
		Command: []string{"claude"},
		Runner:  runner,
		SelfBin: "/usr/local/bin/goobers",
	}
	envelope := testEnvelope(workspace, "contents:read")
	envelope.Inputs = map[string]interface{}{InputArtifactFile: "findings.md"}
	_, err := adapter.Run(context.Background(), RunRequest{
		Envelope:       envelope,
		Workspace:      workspace,
		CompletionPath: DefaultResultPath,
		Credentials:    mcpTestCredentials(t, "contents:read", "local-mcp-secret"),
		MCPServers: []apiv1.MCPServer{{
			Name:    "local-context",
			Command: "context-server",
			CredentialRefs: []apiv1.MCPCredentialRef{
				{Capability: "contents:read", Env: "CONTEXT_TOKEN"},
			},
		}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	command := runner.lastReq.Command

	idx := slices.Index(command, "--mcp-config")
	if idx == -1 {
		t.Fatalf("command missing --mcp-config: %v", command)
	}
	var values []string
	for i := idx + 1; i < len(command) && command[i] != "--strict-mcp-config"; i++ {
		values = append(values, command[i])
	}
	if len(values) != 2 {
		t.Fatalf("--mcp-config values = %v, want 2 (goobers-io + declared servers)", values)
	}
	sawGoobersIO, sawDeclared := false, false
	for _, value := range values {
		if strings.Contains(value, `"goobers-io"`) {
			sawGoobersIO = true
		}
		if _, err := os.Stat(value); err == nil {
			sawDeclared = true
			raw, readErr := os.ReadFile(value)
			if readErr != nil {
				t.Fatal(readErr)
			}
			var config claudeMCPConfig
			if err := json.Unmarshal(raw, &config); err != nil {
				t.Fatal(err)
			}
			local := config.MCPServers["local-context"]
			if local.Type != "stdio" || local.Env["CONTEXT_TOKEN"] != "${GOOBERS_MCP_CREDENTIAL_0_0}" {
				t.Fatalf("declared server config = %#v", local)
			}
		}
	}
	if !sawGoobersIO || !sawDeclared {
		t.Fatalf("expected both goobers-io (inline) and declared (file) values, got: %v", values)
	}
	strictCount := 0
	for _, arg := range command {
		if arg == "--strict-mcp-config" {
			strictCount++
		}
	}
	if strictCount != 1 {
		t.Fatalf("--strict-mcp-config appeared %d times, want exactly 1: %v", strictCount, command)
	}
	if command[idx+1+len(values)] != "--strict-mcp-config" {
		t.Fatalf("--strict-mcp-config does not immediately follow the --mcp-config values: %v", command)
	}

	if !slices.Contains(runner.lastReq.Env, "GOOBERS_MCP_CREDENTIAL_0_0=local-mcp-secret") {
		t.Fatalf("environment missing resolved MCP credential: %v", runner.lastReq.Env)
	}
	for _, arg := range command {
		if strings.Contains(arg, "local-mcp-secret") {
			t.Fatalf("credential leaked into argv: %v", command)
		}
	}
}

// TestClaudeAdapterRunUnaffectedWithoutDeclaredMCPServers pins #1492 AC6's
// claude-code-side analog: a run with no declared mcpServers produces no
// --mcp-config-related change beyond whatever goobers-io itself already
// contributes (none here, since SelfBin is unset).
func TestClaudeAdapterRunUnaffectedWithoutDeclaredMCPServers(t *testing.T) {
	command := runClaudeAdapterForCommand(t, nil)
	if slices.Contains(command, "--mcp-config") || slices.Contains(command, "--strict-mcp-config") {
		t.Fatalf("must not wire MCP config without declared servers or goobers-io: %v", command)
	}
}
