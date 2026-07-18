// Package sandbox provides the platform-neutral seam for confining agentic
// harness subprocesses to their stage workspace.
package sandbox

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

var (
	// ErrUnavailable means the platform sandbox is supported but its required
	// OS tool is not installed.
	ErrUnavailable = errors.New("sandbox unavailable")
	// ErrUnsupported means this operating system has no native sandbox
	// implementation.
	ErrUnsupported = errors.New("sandbox unsupported")
)

// Sandbox rewrites a command so the platform sandbox confines its filesystem
// writes to the policy's roots. The caller retains ownership of command
// environment, stdio, timeout, and process-group configuration.
type Sandbox interface {
	Wrap(command *exec.Cmd, policy Policy) error
}

// Policy declares the stage workspace and any narrow runtime-state directories
// the harness must also write, such as Copilot's resumable session store.
type Policy struct {
	Workspace     string
	WritableRoots []string
}

// New returns the native sandbox for the current operating system.
func New() (Sandbox, error) {
	return newNative()
}

type validatedPolicy struct {
	workspace     string
	writableRoots []string
}

func validate(command *exec.Cmd, policy Policy) (validatedPolicy, error) {
	if command == nil {
		return validatedPolicy{}, fmt.Errorf("sandbox: command is nil")
	}
	if command.Path == "" || len(command.Args) == 0 {
		return validatedPolicy{}, fmt.Errorf("sandbox: command is empty")
	}
	if policy.Workspace == "" {
		return validatedPolicy{}, fmt.Errorf("sandbox: workspace is empty")
	}

	workspace, err := resolveDirectory(policy.Workspace)
	if err != nil {
		return validatedPolicy{}, fmt.Errorf("sandbox: resolve workspace: %w", err)
	}

	if command.Dir == "" {
		command.Dir = workspace
	} else {
		dir, err := resolveDirectory(command.Dir)
		if err != nil {
			return validatedPolicy{}, fmt.Errorf("sandbox: resolve command directory: %w", err)
		}
		relative, err := filepath.Rel(workspace, dir)
		if err != nil {
			return validatedPolicy{}, fmt.Errorf("sandbox: compare command directory to workspace: %w", err)
		}
		if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return validatedPolicy{}, fmt.Errorf("sandbox: command directory %q is outside workspace %q", dir, workspace)
		}
		command.Dir = dir
	}

	validated := validatedPolicy{workspace: workspace}
	for _, root := range policy.WritableRoots {
		resolved, err := resolveDirectory(root)
		if err != nil {
			return validatedPolicy{}, fmt.Errorf("sandbox: resolve writable root %q: %w", root, err)
		}
		if resolved == filepath.VolumeName(resolved)+string(filepath.Separator) {
			return validatedPolicy{}, fmt.Errorf("sandbox: writable root %q cannot be a filesystem root", root)
		}
		validated.writableRoots = append(validated.writableRoots, resolved)
	}
	return validated, nil
}

func resolveDirectory(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%q is not a directory", resolved)
	}
	return resolved, nil
}
