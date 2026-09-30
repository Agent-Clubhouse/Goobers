package harness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

const (
	copilotPreflightHomePrefix = "goobers-copilot-preflight-"
	copilotConfigFileName      = "config.json"
)

func (c *CopilotAdapter) authPreflightPlan(contract launcherContract) ([]string, bool, error) {
	args := c.AuthCheckArgs
	if contract.AuthProbe != nil {
		args = append(slices.Clone(contract.AuthProbe.Args), c.AuthProbeExtraArgs...)
	}
	verifyAdapterManagedSession := c.VerifyAdapterManagedSession &&
		contract.SessionMode == "adapter-managed" &&
		contract.AuthProbe == nil
	if verifyAdapterManagedSession && len(args) == 0 {
		return nil, false, fmt.Errorf("harness: copilot-cli: launcher session verification requires an authentication probe")
	}
	return args, verifyAdapterManagedSession, nil
}

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

func shouldRetryCopilotLauncherAuthProbe(ctx context.Context, result ProcessResult, runErr error, launcherProbe bool) bool {
	if !launcherProbe {
		return false
	}
	if errors.Is(runErr, ErrCanceled) || errors.Is(runErr, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		return false
	}
	return copilotLauncherAuthProbeLooksTransient(ctx, result, runErr)
}

func copilotLauncherAuthProbeLooksTransient(ctx context.Context, result ProcessResult, runErr error) bool {
	if copilotAuthProbeLooksLikeCredentialFailure(result.Transcript) {
		return false
	}
	if errors.Is(runErr, ErrTimeout) || errors.Is(runErr, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return true
	}
	if result.ExitCode == -1 && copilotLauncherAuthProbeProcessStartError(runErr) {
		return true
	}
	return copilotAuthProbeHasLauncherBootstrapSignal(result.Transcript)
}

func copilotLauncherAuthProbeProcessStartError(err error) bool {
	var execErr *exec.Error
	if errors.As(err, &execErr) {
		return true
	}
	var pathErr *os.PathError
	if !errors.As(err, &pathErr) {
		return false
	}
	op := strings.ToLower(pathErr.Op)
	return strings.Contains(op, "exec") || strings.Contains(op, "createprocess")
}

func copilotAuthProbeHasLauncherBootstrapSignal(transcript []byte) bool {
	text := strings.ToLower(string(transcript))
	for _, marker := range []string{
		"bootstrap failed",
		"failed before auth status completed",
		"failed to start session",
		"launcher bootstrap",
		"launcher process failed",
		"mcp bootstrap",
		"mcp startup",
		"session bootstrap",
		"session startup",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func copilotAuthProbeLooksLikeCredentialFailure(transcript []byte) bool {
	text := strings.ToLower(string(transcript))
	for _, marker := range []string{
		"auth failed",
		"authentication failed",
		"credential",
		"forbidden",
		"invalid or revoked",
		"log in",
		"login",
		"not authenticated",
		"not logged in",
		"not signed in",
		"sign in",
		"signed out",
		"token",
		"unauthorized",
		"401",
		"403",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}
