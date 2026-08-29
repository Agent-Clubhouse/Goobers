package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/mcpconfig"
	"github.com/goobers/goobers/internal/safepath"
)

const (
	copilotMCPRuntimeSubdir = ".goobers/mcp"
	copilotMCPRuntimePrefix = "runtime-"
	copilotWorkspaceMCPEnv  = "GITHUB_COPILOT_PROMPT_MODE_WORKSPACE_MCP"
	copilotPluginDirOnlyEnv = "COPILOT_PLUGIN_DIR_ONLY"
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

// prepareCopilotMCP returns the rewritten env, scoping COPILOT_HOME to a
// fresh per-invocation directory for genuinely external MCP servers a
// goober declares (req.MCPServers) — never for goobers-io, which is
// delivered independently; see copilot_mcp_io.go's goobersIORuntimeSubdir
// doc comment for why the two must not share this path.
//
// The runtime root is created before the spawned copilot subprocess is
// sandboxed, inside a workspace that may contain repository-controlled
// content, so it goes through safepath.MkdirLeaf — os.MkdirAll would follow
// a symlink planted at .goobers/mcp or at any not-yet-existing intermediate
// component of it (#2413). Everything below it hangs off the os.MkdirTemp
// directory, whose name no repository content can predict or pre-plant, so
// those creates need no further resolution — but they still use os.Mkdir
// rather than os.MkdirAll so a single component is all any of them creates.
func prepareCopilotMCP(ctx context.Context, req RunRequest, env []string) ([]string, error) {
	if len(req.MCPServers) == 0 {
		return env, nil
	}
	if err := mcpconfig.ValidateForHarness(apiv1.HarnessCopilot, req.MCPServers, req.Envelope.Capabilities, req.Tools); err != nil {
		return nil, fmt.Errorf("harness: copilot-cli: invalid MCP configuration: %w", err)
	}

	runtimeRoot, err := safepath.MkdirLeaf(req.Workspace, filepath.FromSlash(copilotMCPRuntimeSubdir), 0o700)
	if err != nil {
		return nil, fmt.Errorf("harness: copilot-cli: create scoped MCP runtime root: %w", err)
	}
	base, err := os.MkdirTemp(runtimeRoot, copilotMCPRuntimePrefix)
	if err != nil {
		return nil, fmt.Errorf("harness: copilot-cli: create scoped MCP runtime: %w", err)
	}
	home := filepath.Join(base, "copilot-home")
	env = overrideEnv(env, "COPILOT_HOME", home)
	// Keep workspace MCP discovery and ambient plugins from reintroducing
	// servers outside this invocation's generated configuration.
	env = removeEnvironment(env, copilotWorkspaceMCPEnv)
	env = removeEnvironment(env, copilotPluginDirOnlyEnv)
	env = append(env, copilotPluginDirOnlyEnv+"=true")
	if err := os.Mkdir(home, 0o700); err != nil {
		return nil, fmt.Errorf("harness: copilot-cli: create scoped MCP home: %w", err)
	}
	// Ambient config.json may contain OAuth or BYOK credentials that were not
	// resolver-registered, so the scoped home must start empty.
	config := copilotMCPConfig{MCPServers: make(map[string]copilotMCPServer, len(req.MCPServers))}
	for serverIndex, server := range req.MCPServers {
		materialized := copilotMCPServer{
			Tools: append([]string{}, req.Tools...),
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
			key := mcpconfig.CredentialKey(ref)
			token, err := req.Credentials.Token(ctx, key)
			if err != nil {
				return nil, fmt.Errorf("harness: copilot-cli: resolve MCP server %q credential %q: %w", server.Name, key, err)
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

func removeEnvironment(env []string, name string) []string {
	out := env[:0]
	for _, entry := range env {
		entryName, _, _ := strings.Cut(entry, "=")
		if strings.EqualFold(entryName, name) {
			continue
		}
		out = append(out, entry)
	}
	return out
}
