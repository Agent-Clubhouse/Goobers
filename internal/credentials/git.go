package credentials

import (
	"fmt"
	"os"
	"path/filepath"
)

// askpassScriptName is the fixed filename for the askpass helper written
// into a workcopy/worktree's control directory.
const askpassScriptName = "goobers-askpass.sh"

// askpassScript is a secret-free helper: it holds no token. It reads the
// token from an environment variable set only on the git child process, so
// the only place the token ever exists on this machine is that process's
// environment — never a file. GIT_ASKPASS invokes it as
// `goobers-askpass.sh <prompt>`; git's protocol is "print the credential to
// stdout, no trailing newline required but harmless".
const askpassScript = `#!/bin/sh
# Written by internal/credentials (issue #14). Contains no secret material:
# the token is supplied via GOOBERS_GIT_TOKEN on this process's environment.
case "$1" in
  Username*) printf '%s' "${GOOBERS_GIT_USERNAME:-x-access-token}" ;;
  *) printf '%s' "$GOOBERS_GIT_TOKEN" ;;
esac
`

// WriteAskpassScript writes the (secret-free) askpass helper into dir,
// creating dir if needed, and returns its path. It is safe to call
// repeatedly (e.g. once per workcopy); the script is identical every time
// and contains no credential material, so leaving it on disk for the life of
// an ephemeral worktree (SEC-004) carries no exposure.
func WriteAskpassScript(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("credentials: create askpass dir %q: %w", dir, err)
	}
	path := filepath.Join(dir, askpassScriptName)
	if err := os.WriteFile(path, []byte(askpassScript), 0o700); err != nil {
		return "", fmt.Errorf("credentials: write askpass script: %w", err)
	}
	return path, nil
}

// GitEnv returns the environment variables to add to a git child process so
// it authenticates with token via the askpass helper at scriptPath, without
// the token ever being written to disk or appearing on the command line
// (both of which would leak into shell history / process listings / any
// captured harness output). GIT_TERMINAL_PROMPT=0 makes a credential miss
// fail immediately instead of hanging on an interactive prompt — fail
// closed, per ARCHITECTURE.md §2 invariant 6.
func GitEnv(scriptPath, token string) []string {
	return []string{
		"GIT_ASKPASS=" + scriptPath,
		"GOOBERS_GIT_TOKEN=" + token,
		"GIT_TERMINAL_PROMPT=0",
	}
}
