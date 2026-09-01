package worktree

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// InitTargetSafety describes whether a path is suitable for durable Goobers
// instance state. A linked Git worktree is not necessarily disposable, but it
// is the shape used by GitHub-hosted and app-backed sessions, so initialization
// there is refused unless the operator explicitly opts in.
type InitTargetSafety struct {
	Path           string
	RepositoryRoot string
	EphemeralRoot  string
	LinkedWorktree bool
	HostedSession  bool
	Ephemeral      bool
	Reason         string
}

// UnsafeInitTargetError reports a durable initialization target that appears
// to be inside an ephemeral checkout or hosted-agent workspace.
type UnsafeInitTargetError struct {
	Safety   InitTargetSafety
	SafePath string
}

func (e *UnsafeInitTargetError) Error() string {
	return fmt.Sprintf(
		"refusing to initialize Goobers at %s: %s; durable instance state must not live in an ephemeral checkout. Use a safe instance root such as %s, keep checked-in repository configuration in a separate source path, or explicitly acknowledge that this location is intentionally persistent with --allow-ephemeral",
		e.Safety.Path,
		e.Safety.Reason,
		e.SafePath,
	)
}

var initSafetyCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, name, args...)
}

// CheckInitTarget refuses initialization below a linked worktree or a known
// hosted-agent workspace unless allowEphemeral is true. It does not inspect or
// create the target, so callers can run it before any scaffold writes.
func CheckInitTarget(ctx context.Context, target string, allowEphemeral bool) error {
	safety, err := InspectInitTarget(ctx, target)
	if err != nil {
		return err
	}
	if allowEphemeral || !safety.Ephemeral {
		return nil
	}
	return &UnsafeInitTargetError{
		Safety:   safety,
		SafePath: RecommendedInstancePath(safety),
	}
}

// RecommendedInstancePath returns the canonical durable instance location for
// a classified target.
func RecommendedInstancePath(safety InitTargetSafety) string {
	return canonicalSafeInstancePath(safety)
}

// InspectInitTarget classifies target without creating it. Missing target
// components are resolved against their nearest existing ancestor.
func InspectInitTarget(ctx context.Context, target string) (InitTargetSafety, error) {
	absolute, err := filepath.Abs(target)
	if err != nil {
		return InitTargetSafety{}, fmt.Errorf("resolve init target: %w", err)
	}
	path := canonicalInitPath(absolute)
	safety := InitTargetSafety{Path: path}

	if root, linked, ok := inspectGitWorktree(ctx, path); ok && containedPath(root, path) {
		safety.RepositoryRoot = root
		safety.LinkedWorktree = linked
		if linked {
			safety.Ephemeral = true
			safety.EphemeralRoot = root
			safety.Reason = fmt.Sprintf("the target is inside linked Git worktree %s, which may be removed with a GitHub/App session", root)
		}
	}
	if safety.Ephemeral {
		return safety, nil
	}
	if hosted, reason := hostedInitSession(); hosted {
		safety.HostedSession = true
		safety.Ephemeral = true
		safety.Reason = reason
		return safety, nil
	}

	for _, marker := range []struct {
		name  string
		label string
	}{
		{name: "GITHUB_WORKSPACE", label: "GitHub workspace"},
		{name: "RUNNER_TEMP", label: "GitHub runner temporary directory"},
		{name: "RUNNER_WORKSPACE", label: "GitHub runner workspace"},
		{name: "CODESPACE_VSCODE_FOLDER", label: "Codespaces workspace"},
		{name: "COPILOT_WORKSPACE", label: "Copilot workspace"},
		{name: "GITHUB_COPILOT_WORKSPACE", label: "GitHub Copilot workspace"},
	} {
		value := strings.TrimSpace(os.Getenv(marker.name))
		if value == "" {
			continue
		}
		root := canonicalInitPath(value)
		if !containedPath(root, path) {
			continue
		}
		safety.Ephemeral = true
		safety.EphemeralRoot = root
		safety.Reason = fmt.Sprintf("the target is inside %s %s", marker.label, root)
		return safety, nil
	}
	return safety, nil
}

func inspectGitWorktree(ctx context.Context, path string) (root string, linked, ok bool) {
	probe := path
	var nearestRoot string
	for {
		info, err := os.Stat(probe)
		if err == nil {
			if !info.IsDir() {
				probe = filepath.Dir(probe)
				continue
			}
			rootOutput, rootErr := runInitSafetyGit(ctx, probe, "rev-parse", "--show-toplevel")
			if rootErr == nil {
				root = canonicalInitPath(strings.TrimSpace(rootOutput))
				if nearestRoot == "" {
					nearestRoot = root
				}
				gitDir, gitDirErr := runInitSafetyGit(ctx, probe, "rev-parse", "--git-dir")
				commonDir, commonDirErr := runInitSafetyGit(ctx, probe, "rev-parse", "--git-common-dir")
				if gitDirErr == nil && commonDirErr == nil {
					gitDir = canonicalGitPath(probe, strings.TrimSpace(gitDir))
					commonDir = canonicalGitPath(probe, strings.TrimSpace(commonDir))
					linked = !sameInitPath(gitDir, commonDir)
				}
				if linked {
					return root, true, true
				}
				parent := filepath.Dir(root)
				if parent == root {
					return nearestRoot, false, true
				}
				probe = parent
				continue
			}
		} else if !os.IsNotExist(err) {
			break
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			break
		}
		probe = parent
	}
	return nearestRoot, false, nearestRoot != ""
}

// hostedInitSession identifies GitHub's managed runner environment independently
// from its checkout directories. A GitHub-hosted runner's home is destroyed with
// the runner too, so paths outside GITHUB_WORKSPACE are not durable by default.
func hostedInitSession() (bool, string) {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("RUNNER_ENVIRONMENT")), "github-hosted") {
		return true, "this command is running on a GitHub-hosted runner, whose local filesystem is ephemeral"
	}
	return false, ""
}

func runInitSafetyGit(ctx context.Context, directory string, args ...string) (string, error) {
	cmdArgs := append([]string{"-C", directory}, args...)
	output, err := initSafetyCommand(ctx, "git", cmdArgs...).Output()
	if err != nil {
		return "", err
	}
	return string(output), nil
}

func canonicalGitPath(base, value string) string {
	if !filepath.IsAbs(value) {
		value = filepath.Join(base, value)
	}
	return canonicalInitPath(value)
}

func canonicalInitPath(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	current := absolute
	var suffix []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return filepath.Clean(resolved)
		}
		if !os.IsNotExist(err) {
			return filepath.Clean(absolute)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return filepath.Clean(absolute)
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}

func containedPath(root, child string) bool {
	rel, err := filepath.Rel(root, child)
	return err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))))
}

func sameInitPath(first, second string) bool {
	if filepath.Clean(first) == filepath.Clean(second) {
		return true
	}
	if infoFirst, errFirst := os.Stat(first); errFirst == nil {
		if infoSecond, errSecond := os.Stat(second); errSecond == nil {
			return os.SameFile(infoFirst, infoSecond)
		}
	}
	return false
}

func canonicalSafeInstancePath(safety InitTargetSafety) string {
	name := "instance"
	if safety.RepositoryRoot != "" {
		if base := filepath.Base(safety.RepositoryRoot); base != "" && base != "." && base != string(filepath.Separator) {
			name = base
		}
	} else if base := filepath.Base(safety.Path); base != "" && base != "." && base != string(filepath.Separator) {
		name = base
	}
	if safety.HostedSession {
		return filepath.Join("<persistent-volume>", "goobers", "instances", name)
	}
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return filepath.Join("~", "goobers", "instances", name)
	}
	return filepath.Join(home, "goobers", "instances", name)
}
