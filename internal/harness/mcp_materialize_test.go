package harness

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/mcpconfig"
)

func TestMCPHeaderCredentialRenderingAcrossAdapters(t *testing.T) {
	const bearerSecret = "opaque-bearer-material"
	const basicSecret = "opaque-basic-material"

	for _, adapter := range []string{"copilot-cli", "claude-code", "codex"} {
		t.Run(adapter, func(t *testing.T) {
			workspace := t.TempDir()
			req := RunRequest{
				Envelope:  testEnvelope(workspace),
				Workspace: workspace,
				Credentials: mcpTestCredentials(t,
					mcpconfig.BYOCredentialKey("bearer"), bearerSecret,
					mcpconfig.BYOCredentialKey("basic"), basicSecret,
				),
				MCPServers: []apiv1.MCPServer{{
					Name: "remote",
					URL:  "https://mcp.example.test",
					CredentialRefs: []apiv1.MCPCredentialRef{
						{Kind: apiv1.MCPCredentialKindBYO, Ref: "bearer", Header: "Authorization", Scheme: apiv1.MCPHeaderSchemeBearer},
						{Kind: apiv1.MCPCredentialKindBYO, Ref: "basic", Header: "Proxy-Authorization", Scheme: apiv1.MCPHeaderSchemeBasic},
					},
				}},
			}

			var raw []byte
			var env []string
			switch adapter {
			case "copilot-cli":
				var err error
				env, err = prepareCopilotMCP(context.Background(), req, nil)
				if err != nil {
					t.Fatal(err)
				}
				home, _ := environmentValue(env, "COPILOT_HOME")
				raw, err = os.ReadFile(filepath.Join(home, "mcp-config.json"))
				if err != nil {
					t.Fatal(err)
				}
			case "claude-code":
				configPath, additions, err := prepareClaudeMCP(context.Background(), req)
				if err != nil {
					t.Fatal(err)
				}
				env = additions
				raw, err = os.ReadFile(configPath)
				if err != nil {
					t.Fatal(err)
				}
			case "codex":
				configDir := t.TempDir()
				var err error
				env, _, _, err = prepareCodexMCP(context.Background(), req, configDir, "", nil, nil, true)
				if err != nil {
					t.Fatal(err)
				}
				raw, err = os.ReadFile(filepath.Join(configDir, "config.toml"))
				if err != nil {
					t.Fatal(err)
				}
			}

			if bytes.Contains(raw, []byte(bearerSecret)) || bytes.Contains(raw, []byte(basicSecret)) {
				t.Fatalf("%s config contains credential bytes: %s", adapter, raw)
			}
			for _, name := range []string{"GOOBERS_MCP_CREDENTIAL_0_0", "GOOBERS_MCP_CREDENTIAL_0_1"} {
				if !bytes.Contains(raw, []byte(name)) {
					t.Fatalf("%s config does not reference %q: %s", adapter, name, raw)
				}
			}
			if adapter == "codex" {
				if !slices.Contains(env, "GOOBERS_MCP_CREDENTIAL_0_0=Bearer "+bearerSecret) ||
					!slices.Contains(env, "GOOBERS_MCP_CREDENTIAL_0_1=Basic "+basicSecret) {
					t.Fatalf("Codex header credential prefixes changed: %v", redactedNames(env))
				}
				return
			}
			if !bytes.Contains(raw, []byte("Bearer ${GOOBERS_MCP_CREDENTIAL_0_0}")) ||
				!bytes.Contains(raw, []byte("Basic ${GOOBERS_MCP_CREDENTIAL_0_1}")) {
				t.Fatalf("%s placeholder prefixes changed: %s", adapter, raw)
			}
			if !slices.Contains(env, "GOOBERS_MCP_CREDENTIAL_0_0="+bearerSecret) ||
				!slices.Contains(env, "GOOBERS_MCP_CREDENTIAL_0_1="+basicSecret) {
				t.Fatalf("%s environment changed header credential values: %v", adapter, redactedNames(env))
			}
		})
	}
}

func TestCodexMCPMaterializationPreservesConflictErrors(t *testing.T) {
	workspace := t.TempDir()
	base := RunRequest{
		Envelope:  testEnvelope(workspace),
		Workspace: workspace,
		Credentials: mcpTestCredentials(t,
			mcpconfig.BYOCredentialKey("first"), "first-secret",
			mcpconfig.BYOCredentialKey("second"), "second-secret",
		),
	}

	t.Run("generated env conflict", func(t *testing.T) {
		req := base
		req.MCPServers = []apiv1.MCPServer{{
			Name: "remote",
			URL:  "https://mcp.example.test",
			CredentialRefs: []apiv1.MCPCredentialRef{{
				Kind: apiv1.MCPCredentialKindBYO, Ref: "first", Header: "Authorization",
			}},
		}}
		_, _, _, err := prepareCodexMCP(context.Background(), req, t.TempDir(), "", nil, []string{"goobers_mcp_credential_0_0"}, false)
		const want = `harness: codex: generated MCP environment variable "GOOBERS_MCP_CREDENTIAL_0_0" is reserved by the adapter`
		if err == nil || err.Error() != want {
			t.Fatalf("error = %q, want %q", err, want)
		}
	})

	t.Run("local reserved env", func(t *testing.T) {
		req := base
		req.MCPServers = []apiv1.MCPServer{{
			Name:    "local",
			Command: "local-server",
			CredentialRefs: []apiv1.MCPCredentialRef{{
				Kind: apiv1.MCPCredentialKindBYO, Ref: "first", Env: "HOME",
			}},
		}}
		_, _, _, err := prepareCodexMCP(context.Background(), req, t.TempDir(), "", nil, nil, false)
		const want = `harness: codex: MCP environment variable "HOME" is reserved by the adapter`
		if err == nil || err.Error() != want {
			t.Fatalf("error = %q, want %q", err, want)
		}
	})
}

func TestCodexMCPMaterializationPreservesLocalEnvOrder(t *testing.T) {
	workspace := t.TempDir()
	req := RunRequest{
		Envelope:  testEnvelope(workspace),
		Workspace: workspace,
		Credentials: mcpTestCredentials(t,
			mcpconfig.BYOCredentialKey("first"), "first-secret",
			mcpconfig.BYOCredentialKey("second"), "second-secret",
		),
		MCPServers: []apiv1.MCPServer{{
			Name:    "local",
			Command: "local-server",
			CredentialRefs: []apiv1.MCPCredentialRef{
				{Kind: apiv1.MCPCredentialKindBYO, Ref: "first", Env: "Z_TOKEN"},
				{Kind: apiv1.MCPCredentialKindBYO, Ref: "second", Env: "A_TOKEN"},
			},
		}},
	}
	configDir := t.TempDir()
	if _, _, _, err := prepareCodexMCP(context.Background(), req, configDir, "", nil, nil, true); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(configDir, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `env_vars = ["Z_TOKEN", "A_TOKEN"]`) {
		t.Fatalf("local env declaration order changed:\n%s", raw)
	}
}

func TestMCPMaterializationPreservesPreflightErrorPrecedence(t *testing.T) {
	newRequest := func(t *testing.T) RunRequest {
		t.Helper()
		workspace := t.TempDir()
		if err := os.WriteFile(filepath.Join(workspace, ".goobers"), []byte("not a directory"), 0o600); err != nil {
			t.Fatal(err)
		}
		return RunRequest{
			Envelope:  testEnvelope(workspace),
			Workspace: workspace,
			MCPServers: []apiv1.MCPServer{{
				Name: "remote",
				URL:  "https://mcp.example.test",
				CredentialRefs: []apiv1.MCPCredentialRef{{
					Kind: apiv1.MCPCredentialKindBYO, Ref: "missing", Header: "Authorization",
				}},
			}},
		}
	}

	t.Run("copilot scoped runtime before credentials", func(t *testing.T) {
		req := newRequest(t)
		_, err := prepareCopilotMCP(context.Background(), req, nil)
		if err == nil || !strings.HasPrefix(err.Error(), "harness: copilot-cli: create scoped MCP runtime root:") {
			t.Fatalf("error = %q, want scoped runtime root error", err)
		}
		if strings.Contains(err.Error(), "requires credentials") {
			t.Fatalf("credential resolution took precedence over scoped runtime setup: %v", err)
		}
	})

	t.Run("codex automatic goobers-io before credentials", func(t *testing.T) {
		req := newRequest(t)
		req.GoobersIORegistered = true
		_, _, _, err := prepareCodexMCP(context.Background(), req, t.TempDir(), "goobers", nil, nil, false)
		if err == nil || !strings.HasPrefix(err.Error(), "harness: codex: write goobers-io config:") {
			t.Fatalf("error = %q, want automatic goobers-io config error", err)
		}
		if strings.Contains(err.Error(), "requires credentials") {
			t.Fatalf("credential resolution took precedence over automatic goobers-io setup: %v", err)
		}
	})
}
