package harness

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/goobers/goobers/internal/safepath"
)

// isolateInteractiveHome runs after provider authentication/configuration is
// prepared, so changing HOME cannot accidentally select an ambient profile.
// It does not copy user login files, hooks, shell configuration or SSH state.
func isolateInteractiveHome(req RunRequest, env []string) ([]string, error) {
	if !req.IsolateHome {
		return env, nil
	}
	if req.Sandbox == nil {
		return nil, ErrSandboxUnavailable
	}
	for _, dir := range []string{"home", "tmp", "config", "data", "cache"} {
		if _, err := safepath.MkdirLeaf(req.Workspace, filepath.Join(sandboxRuntimeSubdir, dir), 0700); err != nil {
			return nil, fmt.Errorf("interactive runtime directory: %w", err)
		}
	}
	root := filepath.Join(req.Workspace, sandboxRuntimeSubdir)
	env = withoutEnvVars(env, "USERPROFILE", "HOMEDRIVE", "HOMEPATH", "SSH_AUTH_SOCK", "SSH_AGENT_PID", "GIT_ASKPASS", "SSH_ASKPASS")
	for key, dir := range map[string]string{
		"HOME": "home", "TMPDIR": "tmp", "TEMP": "tmp", "TMP": "tmp",
		"XDG_CONFIG_HOME": "config", "XDG_DATA_HOME": "data", "XDG_CACHE_HOME": "cache",
	} {
		env = overrideEnv(env, key, filepath.Join(root, dir))
	}
	env = overrideEnv(env, "GIT_CONFIG_NOSYSTEM", "1")
	env = overrideEnv(env, "GIT_CONFIG_GLOBAL", os.DevNull)
	return env, nil
}
