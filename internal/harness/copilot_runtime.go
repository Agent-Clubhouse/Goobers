package harness

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/goobers/goobers/internal/safepath"
)

// hasCopilotModelCredential examines only the prepared invocation environment.
// A repository GH_TOKEN is not proof of a model grant and must not switch a
// stored-login stage to another identity. Keychain tokenRefs have already been
// resolved by the credential layer, so they follow the same path as env refs.
func (c *CopilotAdapter) hasCopilotModelCredential(env []string) bool {
	name := c.EnvCapabilities["agent:model"]
	if name == "" {
		name = "COPILOT_GITHUB_TOKEN"
	}
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if ok && key == name && value != "" {
			return true
		}
	}
	return false
}

func isolatedCopilotHomeEnv(env []string, home string) []string {
	env = overrideEnv(env, "COPILOT_HOME", home)
	env = removeEnvironment(env, copilotWorkspaceMCPEnv)
	env = removeEnvironment(env, copilotPluginDirOnlyEnv)
	return append(env, copilotPluginDirOnlyEnv+"=true")
}

// prepareCopilotStageHome creates fresh invocation state inside the workspace
// so an existing outer sandbox can write it. No ambient files are read or
// copied; cleanup runs after native telemetry and completion-repair capture.
func prepareCopilotStageHome(workspace string, env []string) ([]string, func(), error) {
	const relative = ".goobers/copilot"
	if _, err := safepath.MkdirLeaf(workspace, filepath.FromSlash(relative), 0o700); err != nil {
		return nil, nil, fmt.Errorf("harness: copilot-cli: prepare private runtime root: %w", err)
	}
	home, err := os.MkdirTemp(filepath.Join(workspace, filepath.FromSlash(relative)), "runtime-")
	if err != nil {
		return nil, nil, fmt.Errorf("harness: copilot-cli: prepare private runtime home: %w", err)
	}
	return isolatedCopilotHomeEnv(env, home), func() { _ = os.RemoveAll(home) }, nil
}
