package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
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
// The root is then re-derived from the caller's own workspace spelling:
// COPILOT_HOME and the generated config path are handed to the subprocess,
// and where the workspace is reached through a symlink (macOS's
// /var -> /private/var, a Windows 8.3 short path) the canonicalized form
// safepath returns would name a location outside the workspace the sandbox
// policy and the caller describe.
func prepareCopilotMCP(ctx context.Context, req RunRequest, env []string) ([]string, error) {
	if len(req.MCPServers) == 0 {
		return env, nil
	}
	opts := mcpMaterializeOptions{
		harness: apiv1.HarnessCopilot,
	}
	if err := validateDeclaredMCP("copilot-cli", req, opts); err != nil {
		return nil, err
	}

	runtimeSubdir := filepath.FromSlash(copilotMCPRuntimeSubdir)
	if _, err := safepath.MkdirLeaf(req.Workspace, runtimeSubdir, 0o700); err != nil {
		return nil, fmt.Errorf("harness: copilot-cli: create scoped MCP runtime root: %w", err)
	}
	runtimeRoot := filepath.Join(req.Workspace, runtimeSubdir)
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
	servers, assignments, err := materializeDeclaredMCP(ctx, "copilot-cli", req, opts)
	if err != nil {
		return nil, err
	}
	config := copilotMCPConfig{MCPServers: make(map[string]copilotMCPServer, len(servers))}
	for _, server := range servers {
		rendered := copilotMCPServer{
			Tools:   append([]string{}, req.Tools...),
			Env:     server.Env,
			Headers: server.Headers,
		}
		if server.Command != "" {
			rendered.Type = "local"
			rendered.Command = server.Command
			rendered.Args = server.Args
		} else {
			rendered.Type = "http"
			rendered.URL = server.URL
		}
		config.MCPServers[server.Name] = rendered
	}
	for _, assignment := range assignments {
		env = overrideEnv(env, assignment.Name, assignment.Value)
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
