package harness

import (
	"context"
	"fmt"
	"path/filepath"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
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
	opts := mcpMaterializeOptions{
		harness: apiv1.HarnessClaudeCode,
	}
	if err := validateDeclaredMCP("claude-code", req, opts); err != nil {
		return "", nil, err
	}
	servers, assignments, err := materializeDeclaredMCP(ctx, "claude-code", req, opts)
	if err != nil {
		return "", nil, err
	}

	config := claudeMCPConfig{MCPServers: make(map[string]claudeMCPServer, len(servers))}
	for _, server := range servers {
		rendered := claudeMCPServer{
			Env:     server.Env,
			Headers: server.Headers,
		}
		if server.Command != "" {
			rendered.Type = "stdio"
			rendered.Command = server.Command
			rendered.Args = server.Args
		} else {
			rendered.Type = "http"
			rendered.URL = server.URL
		}
		config.MCPServers[server.Name] = rendered
	}
	for _, assignment := range assignments {
		envAdditions = append(envAdditions, assignment.Name+"="+assignment.Value)
	}

	configRel := filepath.Join(filepath.FromSlash(claudeMCPRuntimeSubdir), claudeMCPConfigName)
	configPath, err := mcpio.WriteJSON(req.Workspace, configRel, config)
	if err != nil {
		return "", nil, fmt.Errorf("harness: claude-code: write scoped MCP config: %w", err)
	}
	return configPath, envAdditions, nil
}
