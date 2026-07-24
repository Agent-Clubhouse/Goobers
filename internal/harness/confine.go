package harness

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/goobers/goobers/internal/sandbox"
)

// ErrSandboxUnavailable is returned when an agentic stage's effective
// isolation posture is "enforced" but no platform sandbox can confine the
// harness subprocess (unsupported OS, sandbox-exec/bwrap missing or unusable).
// The stage fails closed — it must never run unconfined under an enforced
// posture (S3/#166, ADR-0001), and there is no warn-and-continue downgrade.
var ErrSandboxUnavailable = errors.New("harness: sandbox enforcement is enabled but no platform sandbox is available")

// sandboxRuntimeSubdir is the workspace-relative directory holding the
// per-attempt runtime state a confined Copilot CLI needs to write: its
// COPILOT_HOME (config, session state), a private TMPDIR, and its log
// directory. Keeping them inside the workspace means the policy needs no
// writable roots beyond the worktree for the CLI's own state — the exact
// recipe the sandbox package's live probe codified (ADR-0001: a fresh
// in-worktree COPILOT_HOME per sandboxed run), under the same reserved
// .goobers/ prefix the adapter already uses for prompt.md.
const sandboxRuntimeSubdir = ".goobers/sandbox"

// copilotConfinement is the prepared runtime state for one confined Copilot
// session: the in-workspace directories the CLI writes, plus any writable
// roots outside the workspace the stage legitimately needs (the linked git
// directory of a `git worktree add` workspace — the agent commits its work,
// and those object/ref writes land in the shared mirror, not the worktree).
type copilotConfinement struct {
	copilotHome   string
	tempDir       string
	logDir        string
	writableRoots []string
}

// prepareCopilotConfinement creates the in-workspace runtime directories and
// resolves the workspace's git writable roots.
func prepareCopilotConfinement(workspace string) (*copilotConfinement, error) {
	base := filepath.Join(workspace, filepath.FromSlash(sandboxRuntimeSubdir))
	c := &copilotConfinement{
		copilotHome: filepath.Join(base, "copilot-home"),
		tempDir:     filepath.Join(base, "tmp"),
		logDir:      filepath.Join(base, "logs"),
	}
	for _, dir := range []string{c.copilotHome, c.tempDir, c.logDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create sandbox runtime directory: %w", err)
		}
	}
	roots, err := gitWritableRoots(workspace)
	if err != nil {
		return nil, err
	}
	c.writableRoots = roots
	return c, nil
}

// gitWritableRoots resolves the git directories a stage workspace's git
// operations write outside the workspace itself. A workspace provisioned by
// internal/worktree is a linked worktree of a bare mirror clone: its .git is
// a file naming a gitdir under <mirror>/worktrees/<runID>, and a commit from
// inside the workspace writes the per-worktree gitdir (index, HEAD logs) AND
// the mirror common dir (objects, refs, packed-refs locks). Both must be
// writable or an enforced sandbox would break every implement stage's own
// `git commit`. A workspace with no .git, or with a self-contained .git
// directory, needs no roots beyond the workspace.
func gitWritableRoots(workspace string) ([]string, error) {
	gitPath := filepath.Join(workspace, ".git")
	info, err := os.Lstat(gitPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("inspect workspace git state: %w", err)
	}
	if info.IsDir() {
		return nil, nil
	}
	data, err := os.ReadFile(gitPath)
	if err != nil {
		return nil, fmt.Errorf("read workspace .git file: %w", err)
	}
	gitdir, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir:")
	if !ok {
		return nil, fmt.Errorf("workspace .git file does not declare a gitdir")
	}
	gitdir = strings.TrimSpace(gitdir)
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(workspace, gitdir)
	}
	roots := []string{gitdir}
	// The common dir holds the shared object store and refs; for a linked
	// worktree it is recorded in the gitdir's commondir file, relative to the
	// gitdir. When it contains the gitdir (the usual <mirror>/worktrees/<id>
	// layout), the common dir alone covers both.
	if common, err := os.ReadFile(filepath.Join(gitdir, "commondir")); err == nil {
		commonDir := strings.TrimSpace(string(common))
		if commonDir != "" {
			if !filepath.IsAbs(commonDir) {
				commonDir = filepath.Join(gitdir, commonDir)
			}
			commonDir = filepath.Clean(commonDir)
			if rel, err := filepath.Rel(commonDir, gitdir); err == nil &&
				rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				roots = []string{commonDir}
			} else {
				roots = append(roots, commonDir)
			}
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read gitdir commondir: %w", err)
	}
	// A root inside the workspace is already writable via the workspace rule.
	filtered := roots[:0]
	for _, root := range roots {
		if rel, err := filepath.Rel(workspace, root); err == nil &&
			rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
			continue
		}
		filtered = append(filtered, root)
	}
	return filtered, nil
}

// confineArgv rewrites argv through the platform sandbox so the subprocess's
// filesystem writes are confined to the workspace plus extraRoots. It returns
// the wrapped argv and how many wrapper arguments were prepended (the shift a
// caller must apply to any index it holds into the original argv). The
// sandbox seam rewrites an exec.Cmd in place; the resulting argv is handed to
// the ProcessRunner unchanged, so the runner's session/tree-kill semantics
// (internal/platform/proc) apply to the wrapper exactly as to a bare command.
func confineArgv(sb sandbox.Sandbox, argv []string, workspace string, extraRoots []string) ([]string, int, error) {
	command := exec.Command(argv[0], argv[1:]...)
	command.Dir = workspace
	if err := sb.Wrap(command, sandbox.Policy{Workspace: workspace, WritableRoots: extraRoots}); err != nil {
		return nil, 0, err
	}
	return command.Args, len(command.Args) - len(argv), nil
}

// overrideEnv returns env with any existing name entries removed and a single
// name=value appended — an unambiguous override regardless of how a consumer
// scans the slice (first match or last-wins exec semantics).
func overrideEnv(env []string, name, value string) []string {
	out := env[:0]
	prefix := name + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			continue
		}
		out = append(out, entry)
	}
	return append(out, prefix+value)
}
