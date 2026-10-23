package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// readOnlyGitCommands are the only subcommands the dataset builder and replay
// harness may run against the live repository. None of them writes refs,
// objects, the index, or the working tree.
var readOnlyGitCommands = map[string]bool{
	"archive": true, "blame": true, "diff": true, "log": true,
	"rev-list": true, "rev-parse": true, "show": true,
}

// gitReader runs read-only git commands against a live repository.
type gitReader struct {
	dir string
}

// run executes a read-only git subcommand and returns its stdout. Any other
// subcommand, or an option that writes to a file, is refused.
func (reader gitReader) run(ctx context.Context, args ...string) (string, error) {
	data, err := reader.raw(ctx, args...)
	return string(data), err
}

func (reader gitReader) raw(ctx context.Context, args ...string) ([]byte, error) {
	if len(args) == 0 || !readOnlyGitCommands[args[0]] {
		return nil, fmt.Errorf("refusing git command %q against the live repository: not read-only", strings.Join(args, " "))
	}
	for _, arg := range args[1:] {
		if arg == "-o" || strings.HasPrefix(arg, "--output") {
			return nil, fmt.Errorf("refusing git option %q against the live repository: it writes a file", arg)
		}
	}
	return runGit(ctx, reader.dir, nil, args...)
}

// gitEnvironment strips forge credentials and repository overrides and
// disables prompts and optional locks, so neither the builder nor a replayed
// review can reach a real PR or touch the live repository's index.
func gitEnvironment(extra ...string) []string {
	env := make([]string, 0, len(os.Environ())+len(extra)+2)
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if forgeCredential(name) || name == "GIT_DIR" || name == "GIT_WORK_TREE" || name == "GIT_INDEX_FILE" {
			continue
		}
		env = append(env, entry)
	}
	env = append(env, "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0")
	return append(env, extra...)
}

// forgeCredential reports whether an environment variable carries a
// credential a replayed reviewer could use to post to a real pull request.
func forgeCredential(name string) bool {
	switch strings.ToUpper(name) {
	case "GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "GIT_ASKPASS", "SSH_AUTH_SOCK":
		return true
	}
	return false
}

func runGit(ctx context.Context, dir string, env []string, args ...string) ([]byte, error) {
	full := append([]string{"-C", dir, "-c", "core.quotepath=off", "-c", "gc.auto=0", "-c", "maintenance.auto=0"}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = gitEnvironment(env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}
