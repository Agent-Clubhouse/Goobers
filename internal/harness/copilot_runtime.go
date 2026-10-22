package harness

import (
	"context"
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

// prepareStageRuntime binds sandbox paths and credentialed CLI state before
// session capture paths are derived. The caller retains the home through repair
// and telemetry collection, then invokes cleanup on every exit path.
func (c *CopilotAdapter) prepareStageRuntime(ctx context.Context, req RunRequest, env []string) ([]string, *copilotConfinement, func(), error) {
	// Enforced isolation posture (S3/#166): route the CLI's own runtime state
	// into the workspace, with the exception of raw debug logs, which get an
	// invocation-private external directory and a narrow writable grant below.
	// The remaining runtime state follows ADR-0001. The overrides
	// happen before the session-id block below so the native transcript path
	// derives from the confined COPILOT_HOME.
	var confinement *copilotConfinement
	var err error
	cleanup := func() {}
	if req.Sandbox != nil {
		confinement, err = prepareCopilotConfinement(req.Workspace)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("harness: copilot-cli: sandbox: %w", err)
		}
		env = overrideEnv(env, "COPILOT_HOME", confinement.copilotHome)
		env = overrideEnv(env, "TMPDIR", confinement.tempDir)
	}
	if err := seedTrustedLocalCopilotAuthentication(ctx, env); err != nil {
		return nil, nil, nil, err
	}
	// A resolved model credential lets the stage discard ambient CLI state
	// without migrating renewable login files. Stored-login compatibility
	// keeps the existing home only when no enforced isolation applies.
	if len(req.MCPServers) == 0 && c.hasCopilotModelCredential(env) {
		env, cleanup, err = prepareCopilotStageHome(req.Workspace, env)
		if err != nil {
			return nil, nil, nil, err
		}
	}
	return env, confinement, cleanup, nil
}
