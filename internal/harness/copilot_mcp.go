package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/mcpconfig"
)

// githubMCPServerModule/githubMCPServerPinnedVersion name the prerequisite
// binary in the actionable error below and in docs/guides/github-token-scopes.md
// — keep both in sync with any version bump.
const (
	githubMCPServerModule        = "github.com/github/github-mcp-server/cmd/github-mcp-server"
	githubMCPServerPinnedVersion = "v1.8.0"
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

func prepareCopilotMCP(ctx context.Context, req RunRequest, env []string) ([]string, error) {
	if len(req.MCPServers) == 0 {
		return env, nil
	}
	if err := mcpconfig.ValidateForHarness(apiv1.HarnessCopilot, req.MCPServers, req.Envelope.Capabilities, req.Tools); err != nil {
		return nil, fmt.Errorf("harness: copilot-cli: invalid MCP configuration: %w", err)
	}

	runtimeRoot := filepath.Join(req.Workspace, filepath.FromSlash(copilotMCPRuntimeSubdir))
	if err := os.MkdirAll(runtimeRoot, 0o700); err != nil {
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
	if err := os.MkdirAll(home, 0o700); err != nil {
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

// lookupEnv returns the value of the last name=value entry in env, mirroring
// exec's last-wins semantics.
func lookupEnv(env []string, name string) (string, bool) {
	value, ok := "", false
	prefix := name + "="
	for _, entry := range env {
		if rest, found := strings.CutPrefix(entry, prefix); found {
			value, ok = rest, true
		}
	}
	return value, ok
}

// defaultGithubMCPServerCommand runs the separately-provisioned
// github-mcp-server binary (resolved from PATH) over stdio with every
// toolset registered — --available-tools= (built by copilotAvailableTools)
// is the actual security boundary on top, consistent with #2194's
// --enable-all-github-mcp-tools reasoning: registering broadly is safe
// because it changes what the server *could* expose, not what the model
// *can* call.
var defaultGithubMCPServerCommand = []string{"github-mcp-server", "stdio", "--toolsets=all"}

// githubMCPServerTokenEnv carries this run's GH_TOKEN value into the
// external github-mcp-server subprocess's own environment, referenced from
// the generated MCP config via ${...} expansion rather than embedding the
// raw token in the config file on disk.
const githubMCPServerTokenEnv = "GOOBERS_GITHUB_MCP_SERVER_TOKEN"

// prepareCopilotGithubMCPServer writes a scratch --additional-mcp-config file
// that runs github-mcp-server as an external MCP server authenticated with
// this run's own capability-scoped GH_TOKEN, and returns the updated env plus
// the --additional-mcp-config=@path argument to append to argv (empty if the
// goober does not declare the "github" tool group — no file is written).
//
// #2202: the Copilot CLI's built-in github-mcp-server authenticates with
// COPILOT_GITHUB_TOKEN, not GH_TOKEN, so every real write 403s regardless of
// GH_TOKEN's actual scope. Naming this external server exactly
// "github-mcp-server" makes the CLI defer to it instead of registering its
// own built-in one (confirmed live), so tool identifiers stay
// github-mcp-server-* and copilotToolGroups needs no change.
func (c *CopilotAdapter) prepareCopilotGithubMCPServer(tools []string, workspace string, env []string) ([]string, string, error) {
	if !copilotDeclaresTool(tools, "github") {
		return env, "", nil
	}
	token, ok := lookupEnv(env, "GH_TOKEN")
	if !ok || token == "" {
		return nil, "", fmt.Errorf("harness: copilot-cli: goober declares the github tool group but no GH_TOKEN credential was materialized for this run")
	}
	command := c.GithubMCPServerCommand
	if len(command) == 0 {
		command = defaultGithubMCPServerCommand
	}
	if _, err := exec.LookPath(command[0]); err != nil {
		return nil, "", fmt.Errorf("harness: copilot-cli: %q not found on PATH — install it: "+
			"go install %s@%s (github.com/github/github-mcp-server)", command[0], githubMCPServerModule, githubMCPServerPinnedVersion)
	}
	config := copilotMCPConfig{MCPServers: map[string]copilotMCPServer{
		"github-mcp-server": {
			Type:    "local",
			Command: command[0],
			Args:    append([]string(nil), command[1:]...),
			// Empty (non-nil — the field has no omitempty and Copilot
			// rejects a null "tools" value): confirmed live this exposes
			// every tool the server itself registers, same as omitting a
			// per-server allowlist. --available-tools= below is what
			// actually scopes the model-visible set.
			Tools: []string{},
			Env:   map[string]string{"GITHUB_PERSONAL_ACCESS_TOKEN": "${" + githubMCPServerTokenEnv + "}"},
		},
	}}
	data, err := json.Marshal(config)
	if err != nil {
		return nil, "", fmt.Errorf("harness: copilot-cli: encode github MCP server config: %w", err)
	}
	dir := filepath.Join(workspace, filepath.FromSlash(".goobers/mcp"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, "", fmt.Errorf("harness: copilot-cli: create github MCP config dir: %w", err)
	}
	path := filepath.Join(dir, "github-mcp-server.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return nil, "", fmt.Errorf("harness: copilot-cli: write github MCP server config: %w", err)
	}
	env = overrideEnv(env, githubMCPServerTokenEnv, token)
	return env, "--additional-mcp-config=@" + path, nil
}
