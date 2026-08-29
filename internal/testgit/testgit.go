// Package testgit provides isolated Git commands for repository fixtures.
package testgit

import (
	"context"
	"os"
	"os/exec"
	"strings"
)

var configArgs = []string{"-c", "gc.auto=0", "-c", "maintenance.auto=0"}

var isolatedEnvironment = map[string]string{
	"GIT_CONFIG_GLOBAL":   os.DevNull,
	"GIT_CONFIG_SYSTEM":   os.DevNull,
	"GIT_CONFIG_NOSYSTEM": "1",
	"GIT_TERMINAL_PROMPT": "0",
	"GIT_OPTIONAL_LOCKS":  "0",
}

// Command creates an isolated git command for use by tests.
func Command(args ...string) *exec.Cmd {
	cmd := exec.Command("git", appendArgs(args)...)
	cmd.Env = Environment()
	return cmd
}

// CommandContext creates an isolated git command for use by tests.
func CommandContext(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", appendArgs(args)...)
	cmd.Env = Environment()
	return cmd
}

// AmbientCommand is reserved for tests that explicitly verify the git
// environment inherited by subprocesses.
func AmbientCommand(args ...string) *exec.Cmd {
	return exec.Command("git", args...)
}

// Environment returns the process environment with host git configuration
// replaced by deterministic test settings.
func Environment() []string {
	return IsolateEnvironment(os.Environ())
}

// IsolateEnvironment applies deterministic git settings to an existing
// environment, such as one containing fixture-specific credentials.
func IsolateEnvironment(base []string) []string {
	env := make([]string, 0, len(base)+len(isolatedEnvironment))
	for _, entry := range base {
		name, _, _ := strings.Cut(entry, "=")
		if _, isolated := isolatedEnvironment[name]; !isolated {
			env = append(env, entry)
		}
	}
	for name, value := range isolatedEnvironment {
		env = append(env, name+"="+value)
	}
	return env
}

func appendArgs(args []string) []string {
	full := make([]string, 0, len(configArgs)+len(args))
	full = append(full, configArgs...)
	return append(full, args...)
}
