package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/mcpconfig"
)

const (
	copilotMCPRuntimePrefix = ".goobers-mcp-"
	copilotWorkspaceMCPEnv  = "GITHUB_COPILOT_PROMPT_MODE_WORKSPACE_MCP"
)

type copilotMCPConfig struct {
	MCPServers map[string]copilotMCPServer `json:"mcpServers"`
}

type copilotMCPServer struct {
	Type    string            `json:"type"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	URL     string            `json:"url,omitempty"`
	Tools   []string          `json:"tools"`
	Env     map[string]string `json:"env,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

func prepareCopilotMCP(ctx context.Context, req RunRequest, env []string) ([]string, error) {
	if len(req.MCPServers) == 0 {
		return env, nil
	}
	if err := mcpconfig.Validate(req.MCPServers, req.Envelope.Capabilities); err != nil {
		return nil, fmt.Errorf("harness: copilot-cli: invalid MCP configuration: %w", err)
	}

	base, err := os.MkdirTemp(req.Workspace, copilotMCPRuntimePrefix)
	if err != nil {
		return nil, fmt.Errorf("harness: copilot-cli: create scoped MCP runtime: %w", err)
	}
	home := filepath.Join(base, "copilot-home")
	env = overrideEnv(env, "COPILOT_HOME", home)
	// Prompt mode normally disables workspace MCP discovery, but an ambient
	// opt-in must not re-enable undeclared repo configuration for this session.
	env = removeEnvironment(env, copilotWorkspaceMCPEnv)
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, fmt.Errorf("harness: copilot-cli: create scoped MCP home: %w", err)
	}

	config := copilotMCPConfig{MCPServers: make(map[string]copilotMCPServer, len(req.MCPServers))}
	for serverIndex, server := range req.MCPServers {
		materialized := copilotMCPServer{
			Tools: []string{"*"},
		}
		if server.Command != "" {
			materialized.Type = "local"
			materialized.Command = server.Command
			materialized.Args = append([]string(nil), server.Args...)
		} else {
			materialized.Type = "http"
			materialized.URL = server.URL
		}
		for refIndex, ref := range server.CredentialRefs {
			if req.Credentials == nil {
				return nil, fmt.Errorf("harness: copilot-cli: MCP server %q requires credentials but none were materialized", server.Name)
			}
			token, err := req.Credentials.Token(ctx, ref.Capability)
			if err != nil {
				return nil, fmt.Errorf("harness: copilot-cli: resolve MCP server %q credential %q: %w", server.Name, ref.Capability, err)
			}
			envName := fmt.Sprintf("GOOBERS_MCP_CREDENTIAL_%d_%d", serverIndex, refIndex)
			env = overrideEnv(env, envName, token)
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

	data, err := json.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("harness: copilot-cli: encode scoped MCP config: %w", err)
	}
	if err := os.WriteFile(filepath.Join(home, "mcp-config.json"), data, 0o600); err != nil {
		return nil, fmt.Errorf("harness: copilot-cli: write scoped MCP config: %w", err)
	}
	return env, nil
}

func environmentValue(env []string, name string) (string, bool) {
	prefix := name + "="
	for _, entry := range env {
		if len(entry) >= len(prefix) && entry[:len(prefix)] == prefix {
			return entry[len(prefix):], true
		}
	}
	return "", false
}

func removeEnvironment(env []string, name string) []string {
	out := env[:0]
	prefix := name + "="
	for _, entry := range env {
		if len(entry) >= len(prefix) && entry[:len(prefix)] == prefix {
			continue
		}
		out = append(out, entry)
	}
	return out
}
