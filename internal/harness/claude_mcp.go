package harness

import (
	"context"
	"fmt"
	"path/filepath"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/mcpconfig"
	"github.com/goobers/goobers/internal/mcpio"
)

const (
	claudeMCPRuntimeSubdir = ".goobers/mcp"
	claudeMCPConfigName    = "claude-mcp-config.json"
)

type claudeMCPConfig struct {
	MCPServers map[string]claudeMCPServer `json:"mcpServers"`
}

// claudeMCPServer has no per-server "tools" sub-allowlist field the way
// copilotMCPServer does: confirmed live (#2774, reconfirmed here) that
// claude-code's --tools/--allowedTools don't gate MCP-server tools at all —
// once a server is registered, every tool it reports is reachable regardless
// of the goober's declared Spec.Tools. This is an accepted, unavoidable
// adapter-parity gap (no CLI mechanism exists to close it), not something
// this materializer can enforce.
type claudeMCPServer struct {
	Type    string            `json:"type"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	URL     string            `json:"url,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// prepareClaudeMCP materializes a goober's declared external MCP servers
// (req.MCPServers) for claude-code — never for goobers-io, which is
// delivered independently via claude_mcp_io.go; see that file's doc comment
// for why the two paths must not merge. It mirrors prepareCopilotMCP's
// credential-placeholder strategy: confirmed live that claude-code's
// --mcp-config supports the same "${VAR}" expansion Copilot's mcp-config.json
// relies on, so a resolved credential is threaded in as a real environment
// variable (returned in envAdditions, for the caller to layer onto the
// subprocess environment) while only a "${GOOBERS_MCP_CREDENTIAL_i_j}"
// placeholder — never the secret itself — is written to the config file or
// appears in the CLI's argv.
//
// Returns ("", nil, nil) when req.MCPServers is empty. The config write uses
// mcpio.WriteJSON (symlink-safe), not a raw os.MkdirAll+os.WriteFile: this
// runs in the harness's own process, before the spawned claude subprocess is
// sandboxed, against a workspace that may contain repository-controlled
// content (the same #2413-class concern documented on the Copilot and
// goobers-io config writes).
func prepareClaudeMCP(ctx context.Context, req RunRequest) (mcpConfigArg string, envAdditions []string, err error) {
	if len(req.MCPServers) == 0 {
		return "", nil, nil
	}
	if err := mcpconfig.ValidateForHarness(apiv1.HarnessClaudeCode, req.MCPServers, req.Envelope.Capabilities, req.Tools); err != nil {
		return "", nil, fmt.Errorf("harness: claude-code: invalid MCP configuration: %w", err)
	}

	config := claudeMCPConfig{MCPServers: make(map[string]claudeMCPServer, len(req.MCPServers))}
	for serverIndex, server := range req.MCPServers {
		materialized := claudeMCPServer{}
		if server.Command != "" {
			materialized.Type = "stdio"
			materialized.Command = server.Command
			materialized.Args = append([]string(nil), server.Args...)
		} else {
			materialized.Type = "http"
			materialized.URL = server.URL
		}
		for refIndex, ref := range server.CredentialRefs {
			if req.Credentials == nil {
				return "", nil, fmt.Errorf("harness: claude-code: MCP server %q requires credentials but none were materialized", server.Name)
			}
			key := mcpconfig.CredentialKey(ref)
			token, err := req.Credentials.Token(ctx, key)
			if err != nil {
				return "", nil, fmt.Errorf("harness: claude-code: resolve MCP server %q credential %q: %w", server.Name, key, err)
			}
			envName := fmt.Sprintf("GOOBERS_MCP_CREDENTIAL_%d_%d", serverIndex, refIndex)
			envAdditions = append(envAdditions, envName+"="+token)
			expansion := "${" + envName + "}"
			if ref.Env != "" {
				if materialized.Env == nil {
					materialized.Env = make(map[string]string)
				}
				materialized.Env[ref.Env] = expansion
				continue
			}
			if materialized.Headers == nil {
				materialized.Headers = make(map[string]string)
			}
			switch ref.Scheme {
			case apiv1.MCPHeaderSchemeBearer:
				expansion = "Bearer " + expansion
			case apiv1.MCPHeaderSchemeBasic:
				expansion = "Basic " + expansion
			}
			materialized.Headers[ref.Header] = expansion
		}
		config.MCPServers[server.Name] = materialized
	}

	configRel := filepath.Join(filepath.FromSlash(claudeMCPRuntimeSubdir), claudeMCPConfigName)
	configPath, err := mcpio.WriteJSON(req.Workspace, configRel, config)
	if err != nil {
		return "", nil, fmt.Errorf("harness: claude-code: write scoped MCP config: %w", err)
	}
	return configPath, envAdditions, nil
}
