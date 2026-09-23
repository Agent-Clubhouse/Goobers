package harness

import (
	"fmt"
	"os"
	"path/filepath"
)

const (
	copilotPreflightHomePrefix = "goobers-copilot-preflight-"
	copilotConfigFileName      = "config.json"
)

// prepareCopilotPreflightEnvironment isolates the authentication probe from
// ambient extensions, plugins, MCP servers, hooks, and instructions. When no
// explicit model token is available, config.json is copied so the CLI can
// still use its stored login without inheriting the rest of the user profile.
func prepareCopilotPreflightEnvironment(env []string, seedStoredAuth bool) ([]string, string, func(), error) {
	ambientHome, hasAmbientHome := copilotConfigHome(env)
	home, err := os.MkdirTemp("", copilotPreflightHomePrefix)
	if err != nil {
		return nil, "", nil, fmt.Errorf("create isolated Copilot preflight home: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(home) }

	if seedStoredAuth && hasAmbientHome {
		source := filepath.Join(ambientHome, copilotConfigFileName)
		data, readErr := os.ReadFile(source)
		switch {
		case readErr == nil:
			if err := os.WriteFile(filepath.Join(home, copilotConfigFileName), data, 0o600); err != nil {
				cleanup()
				return nil, "", nil, fmt.Errorf("seed isolated Copilot preflight authentication: %w", err)
			}
		case !os.IsNotExist(readErr):
			cleanup()
			return nil, "", nil, fmt.Errorf("read stored Copilot authentication: %w", readErr)
		}
	}

	env = overrideEnv(env, "COPILOT_HOME", home)
	env = removeEnvironment(env, copilotWorkspaceMCPEnv)
	env = removeEnvironment(env, copilotPluginDirOnlyEnv)
	env = append(env, copilotPluginDirOnlyEnv+"=true")
	return env, home, cleanup, nil
}
