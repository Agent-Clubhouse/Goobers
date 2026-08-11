// Package mcpio implements the "goobers-io" MCP server: a generic,
// workflow-agnostic replacement for the file-write step of an agentic
// stage's completion contract. A goober calls publish_output instead of
// writing a file with a generic editing tool — see #2406. Each harness
// adapter (internal/harness's copilot_mcp_io.go for Copilot,
// claude_mcp_io.go for claude-code — #2774) writes this server's Config to a
// workspace-relative path before invocation, then registers it with its own
// CLI's MCP flag (Copilot's --additional-mcp-config, claude-code's
// --mcp-config) with that path passed to the spawned process as --config —
// deliberately not tied to any adapter-specific config-home directory, so
// this works whether or not the invocation has stored-login auth or any
// other MCP server configured (a prior COPILOT_HOME-relative design broke
// both, for Copilot). This package only ever reads the config file it's
// told about; it never talks to the journal or the runner directly, and has
// no awareness of which adapter is driving it.
package mcpio

import (
	"encoding/json"
	"fmt"
	"os"
)

// ConfigFileName is the fixed filename the harness writes this server's
// config to, inside its own workspace-relative runtime directory.
const ConfigFileName = "goobers-io-config.json"

// Config is what the harness writes before invocation. Workspace is the
// stage's own worktree; the identity fields come from its invocation envelope;
// ArtifactFile is empty when the task declares no artifactFile input
// (publish_output is then unavailable, not silently a no-op); Inputs maps a
// human-readable name to a workspace-relative path already materialized by
// materializeContext.
type Config struct {
	Workspace    string            `json:"workspace"`
	ArtifactFile string            `json:"artifactFile,omitempty"`
	Inputs       map[string]string `json:"inputs,omitempty"`
	RunID        string            `json:"runId"`
	WorkflowID   string            `json:"workflowId"`
	TaskID       string            `json:"taskId"`
	Gaggle       string            `json:"gaggle"`
}

// LoadConfig reads and validates a config file at path.
func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("mcpio: read config %s: %w", path, err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("mcpio: parse config %s: %w", path, err)
	}
	if cfg.Workspace == "" {
		return Config{}, fmt.Errorf("mcpio: config %s declares no workspace", path)
	}
	return cfg, nil
}

// WriteConfig writes cfg to path, creating parent directories as needed.
// Called from the harness side (internal/harness), not by this server
// itself.
// WriteConfig resolves rel against root (a task's own worktree, which may
// contain repository-controlled content) and writes cfg there, returning
// the resolved absolute path. This runs in the harness's own process,
// before the spawned copilot subprocess is sandboxed — so, unlike a normal
// os.MkdirAll+os.WriteFile, it must not follow a symlink planted at rel or
// any not-yet-existing intermediate component of it (see resolveRooted's
// doc comment and #2413, which tracks the same gap at other pre-sandbox
// harness writes into a workspace).
func WriteConfig(root, rel string, cfg Config) (string, error) {
	data, err := json.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("mcpio: encode config: %w", err)
	}
	full, err := resolveRooted(root, rel, true)
	if err != nil {
		return "", fmt.Errorf("mcpio: resolve config path: %w", err)
	}
	if err := os.WriteFile(full, data, 0o600); err != nil {
		return "", fmt.Errorf("mcpio: write config %s: %w", full, err)
	}
	return full, nil
}
