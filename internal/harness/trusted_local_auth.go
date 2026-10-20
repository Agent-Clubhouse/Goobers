package harness

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/platform/secfile"
)

func trustedLocalCodexEnvironment(ctx context.Context, env []string, options codexConfig) ([]string, error) {
	auth, ok := credentials.TrustedLocalAuthenticationFor(ctx, "codex")
	if !ok || options.allowFileBackedCredentials && !auth.AllowFileBacked {
		return nil, credentials.ErrChildAuthenticationIsolation
	}
	// Codex's OS keyring identity depends on CODEX_HOME. Retain only the
	// delegated directory; exec still ignores user config, hooks and rules,
	// and keyring-only parents cannot delegate auth.json-backed sessions.
	env = withoutEnvVars(env, codexModelEnv)
	return overrideEnv(env, "CODEX_HOME", auth.Home), nil
}

func seedTrustedLocalCopilotAuthentication(ctx context.Context, env []string) error {
	if !credentials.TrustedLocalProcess(ctx) {
		return nil
	}
	if token, _ := environmentValue(env, "COPILOT_GITHUB_TOKEN"); token != "" {
		return nil
	}
	auth, ok := credentials.TrustedLocalAuthenticationFor(ctx, "copilot")
	if !ok {
		return credentials.ErrChildAuthenticationIsolation
	}
	home, ok := copilotConfigHome(env)
	if !ok || home == auth.Home {
		return credentials.ErrChildAuthenticationIsolation
	}
	// Reuse the same selected login file as isolated Copilot preflight.
	// Do not copy MCP registrations, plugins, hooks or other CLI homes.
	data, err := os.ReadFile(filepath.Join(auth.Home, copilotConfigFileName))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil // The selected CLI may resolve its OS-keyring login.
	}
	return secfile.WritePrivate(filepath.Join(home, copilotConfigFileName), data)
}
