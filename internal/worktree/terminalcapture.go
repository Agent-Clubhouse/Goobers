package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// terminalCaptureDirectory is the single deterministic scratch checkout
// WithRunBranchCheckout materializes under a repository's managed directory.
// One fixed name — the rationale initializeRecoveryMirror's staging directory
// uses — bounds the debris an interrupted capture can leave to exactly one
// directory that the next capture reclaims, instead of growing a new random
// path per crash.
const terminalCaptureDirectory = "terminal-capture"

// RepositoryKey is the managed directory name under a Manager's root for a
// repository clone URL. Terminal recovery needs it to attribute the mirror it
// found a run branch in back to a configured repository identity.
func RepositoryKey(repoURL string) string { return repoKey(repoURL) }

// WithRunBranchCheckout finds the managed mirror holding branch, visits a
// temporary detached checkout of that branch's tip under the repository lock,
// and reports whether any mirror had the branch at all. visit receives the
// mirror's managed key, the checkout path, and the tip it is detached at.
//
// It is the terminal-time counterpart to a stage worktree: `git worktree
// remove` never deletes a branch, so a run whose stage worktrees were all torn
// down while it was still nonterminal still has its committed implementation
// here, and nothing else can reach it.
//
// The search is over the managed mirrors themselves rather than over a
// caller-supplied repository, so the cheap "this run has nothing on disk to
// protect" answer is available BEFORE any configuration is resolved. A run
// branch name embeds the run ID, so at most one mirror can hold it; mirrors are
// visited in sorted order so a pathological duplicate resolves deterministically.
//
// The checkout is created and destroyed with the lowest-level git worktree
// operations deliberately. Manager.Create and Manager.Remove run the cleanup
// guards, and the recovery guard is exactly this function's caller, so routing
// a capture scratch tree through them would recurse: the guard would try to
// publish recovery for the throwaway checkout it was invoked to produce. This
// path writes no marker and no ownership record, so no guard, reaper or
// retention sweep ever sees it as an owned worktree.
func (m *Manager) WithRunBranchCheckout(ctx context.Context, branch string, visit func(key, path, tip string) error) (bool, error) {
	if visit == nil {
		return false, fmt.Errorf("worktree: run-branch checkout requires a visitor")
	}
	if err := validRunBranch(branch); err != nil {
		// A run whose journalled branch name is unusable has nothing this can
		// safely act on. That is an absence, not a capture failure.
		return false, nil
	}
	keys, err := managedRepositoryKeys(m.Root)
	if err != nil || len(keys) == 0 {
		return false, err
	}
	for _, key := range keys {
		found, err := m.visitRunBranchInMirror(ctx, key, branch, visit)
		if found || err != nil {
			return found, err
		}
	}
	return false, nil
}

// managedRepositoryKeys lists the managed repository directories under root. A
// missing root is an empty list: an instance that never provisioned a mirror
// cannot be holding a run branch.
func managedRepositoryKeys(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("worktree: list managed repositories: %w", err)
	}
	var keys []string
	for _, entry := range entries {
		if entry.IsDir() {
			keys = append(keys, entry.Name())
		}
	}
	sort.Strings(keys)
	return keys, nil
}

func (m *Manager) visitRunBranchInMirror(ctx context.Context, key, branch string, visit func(key, path, tip string) error) (bool, error) {
	lock := m.lockFor(key)
	lock.Lock()
	defer lock.Unlock()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	dir := m.repoDirForKey(key)
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
		return false, nil
	}
	tip, err := runBranchTip(ctx, dir, branch)
	if err != nil || tip == "" {
		// An unreadable mirror is not this run's work going missing; the
		// branch simply cannot be shown to be here.
		return false, nil
	}
	path := filepath.Join(m.Root, key, terminalCaptureDirectory)
	if err := clearTerminalCapture(ctx, dir, path); err != nil {
		return true, err
	}
	if err := runGit(ctx, dir, "worktree", "add", "--detach", "--", path, tip); err != nil {
		return true, fmt.Errorf("worktree: check out run branch %q for terminal recovery: %w", branch, err)
	}
	visitErr := visit(key, path, tip)
	return true, errors.Join(visitErr, clearTerminalCapture(ctx, dir, path))
}

// runBranchTip resolves branch's local tip in the mirror. An absent branch is
// not an error: a run that never created one simply has nothing to capture.
func runBranchTip(ctx context.Context, dir, branch string) (string, error) {
	out, err := rawGitOutput(ctx, dir, nil, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch+"^{commit}")
	if err != nil {
		var gitErr *gitCommandError
		if errors.As(err, &gitErr) && gitErr.exitCode == 1 {
			return "", nil
		}
		return "", fmt.Errorf("worktree: resolve run branch %q: %w", branch, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// clearTerminalCapture makes the scratch checkout absent and unregistered,
// whether it is this call's own or one an interrupted earlier capture left
// behind. Both git steps are best-effort because neither has anything to do in
// the common case; the directory removal is what must succeed.
func clearTerminalCapture(ctx context.Context, dir, path string) error {
	_ = runGit(ctx, dir, "worktree", "remove", "--force", "--", path)
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("worktree: remove terminal capture checkout: %w", err)
	}
	_ = runGit(ctx, dir, "worktree", "prune")
	return nil
}

// validRunBranch refuses anything that is not a plain branch name, so a
// corrupt journal cannot turn a capture into an option-bearing git invocation
// or a traversal out of the managed mirror.
func validRunBranch(branch string) error {
	if branch == "" || strings.HasPrefix(branch, "-") || strings.HasPrefix(branch, "/") ||
		strings.HasSuffix(branch, "/") || strings.Contains(branch, "..") ||
		strings.ContainsAny(branch, " \t\x00\r\n:?*[\\~^") {
		return fmt.Errorf("worktree: %q is not a usable run branch name", branch)
	}
	return nil
}
