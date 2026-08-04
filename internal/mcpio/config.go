// Package mcpio implements the "goobers-io" MCP server: a generic, workflow-
// agnostic replacement for the file-write step of an agentic stage's
// completion contract. A goober declares it via mcpServers (api/v1alpha1's
// MCPServer) and calls publish_output instead of writing a file with a
// generic editing tool — see #2406. The harness writes this server's Config
// into $COPILOT_HOME/goobers-io-config.json before invocation (see
// internal/harness's copilot_mcp_io.go); this package only ever reads that
// file, it never talks to the journal or the runner directly.
package mcpio

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ConfigFileName is the fixed filename this server's config lives at, always
// relative to COPILOT_HOME — the same directory prepareCopilotMCP already
// scopes fresh per invocation, so no other coordination between the harness
// and this process is needed.
const ConfigFileName = "goobers-io-config.json"

// Config is what the harness writes before invocation. Workspace is the
// stage's own worktree; ArtifactFile is empty when the task declares no
// artifactFile input (publish_output is then unavailable, not silently a
// no-op); Inputs maps a human-readable name to a workspace-relative path
// already materialized by materializeContext.
type Config struct {
	Workspace    string            `json:"workspace"`
	ArtifactFile string            `json:"artifactFile,omitempty"`
	Inputs       map[string]string `json:"inputs,omitempty"`
}

// LoadConfigFromEnv resolves COPILOT_HOME from the process environment and
// reads ConfigFileName from it. A missing COPILOT_HOME or config file is a
// startup error — this process has no other way to know its workspace.
func LoadConfigFromEnv() (Config, error) {
	home := os.Getenv("COPILOT_HOME")
	if home == "" {
		return Config{}, fmt.Errorf("mcpio: COPILOT_HOME is not set")
	}
	return LoadConfig(filepath.Join(home, ConfigFileName))
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

// WriteConfig writes cfg to path (COPILOT_HOME/goobers-io-config.json),
// creating parent directories as needed. Called from the harness side
// (internal/harness), not by this server itself.
func WriteConfig(path string, cfg Config) error {
	data, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("mcpio: encode config: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mcpio: create config dir: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("mcpio: write config %s: %w", path, err)
	}
	return nil
}
