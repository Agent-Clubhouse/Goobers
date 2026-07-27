package credentials

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// askpassScriptName is the fixed filename for the POSIX askpass helper
// written into a workcopy/worktree's control directory.
const askpassScriptName = "goobers-askpass.sh"

// askpassScriptNameWindows is the fixed filename for the Windows askpass
// helper written into a workcopy/worktree's control directory.
const askpassScriptNameWindows = "goobers-askpass.cmd"

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

// askpassScriptWindows is a secret-free helper for Windows cmd.exe-based
// GIT_ASKPASS execution. It mirrors the POSIX askpassScript contract exactly:
// a "Username" prompt is answered from GOOBERS_GIT_USERNAME (defaulting to
// x-access-token), every other prompt from GOOBERS_GIT_TOKEN. Answering the
// Username prompt with the raw token (as an earlier version did) diverges from
// that contract and leaks the token into git's username field.
const askpassScriptWindows = `@echo off
echo(%~1| findstr /B /I /C:"Username" >nul
if not errorlevel 1 (
  if defined GOOBERS_GIT_USERNAME (echo(%GOOBERS_GIT_USERNAME%) else (echo x-access-token)
) else (
  echo(%GOOBERS_GIT_TOKEN%
)
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

	if runtime.GOOS == "windows" {
		return writeAskpassFile(dir, askpassScriptNameWindows, askpassScriptWindows)
	}
	return writeAskpassFile(dir, askpassScriptName, askpassScript)
}

func writeAskpassFile(dir, name, content string) (string, error) {
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
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

// GitAuthEnvironment returns the COMPLETE child environment for a git network
// command authenticated via the askpass helper at scriptPath — for callers
// that replace a subprocess's environment wholesale (cmd.Env) rather than
// appending to it, e.g. worktree.Manager's remote clone/fetch (#667). It is
// the current process environment with two adjustments:
//
//   - Ambient credential configuration is removed and git's credential-helper
//     chain is disabled (GIT_CONFIG_* env config, git 2.31+). Git consults
//     helpers BEFORE falling back to askpass, so an operator's keychain or
//     store helper would otherwise silently shadow the configured token with
//     whatever identity the host happens to hold.
//   - GitEnv's askpass variables are appended, so the token exists only in
//     the child process environment — never on disk or argv.
func GitAuthEnvironment(scriptPath, token string) []string {
	env := make([]string, 0, len(os.Environ())+6)
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		upper := strings.ToUpper(name)
		if upper == "GIT_ASKPASS" || upper == "GIT_TERMINAL_PROMPT" ||
			upper == "GOOBERS_GIT_TOKEN" || upper == "GOOBERS_GIT_USERNAME" ||
			upper == "GIT_CONFIG_COUNT" ||
			strings.HasPrefix(upper, "GIT_CONFIG_KEY_") || strings.HasPrefix(upper, "GIT_CONFIG_VALUE_") {
			continue
		}
		env = append(env, entry)
	}
	env = append(env,
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=credential.helper",
		"GIT_CONFIG_VALUE_0=",
	)
	return append(env, GitEnv(scriptPath, token)...)
}
