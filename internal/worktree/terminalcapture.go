package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// terminalCaptureDirectory is the single deterministic scratch checkout
// WithRunBranchCheckout materializes under a repository's managed directory.
// One fixed name — the rationale initializeRecoveryMirror's staging directory
// uses — bounds the debris an interrupted capture can leave to exactly one
// directory that the next capture reclaims, instead of growing a new random
// path per crash.
const terminalCaptureDirectory = "terminal-capture"

// WithRunBranchCheckout visits a temporary detached checkout of branch's tip in
// the managed mirror for repoURL, under the repository lock, and reports
// whether that branch existed at all. It is the terminal-time counterpart to a
// stage worktree: `git worktree remove` never deletes a branch, so a run whose
// stage worktrees were all torn down while it was still nonterminal still has
// its committed implementation here, and nothing else can reach it.
//
// The checkout is created and destroyed with the lowest-level git worktree
// operations deliberately. Manager.Create and Manager.Remove run the cleanup
// guards, and the recovery guard is exactly this function's caller, so routing
// a capture scratch tree through them would recurse: the guard would try to
// publish recovery for the throwaway checkout it was invoked to produce. This
// path writes no marker and no ownership record, so no guard, reaper or
// retention sweep ever sees it as an owned worktree.
//
// visit receives the checkout path and the branch tip it is detached at. Its
// error is returned verbatim so the caller can fail closed on it.
func (m *Manager) WithRunBranchCheckout(ctx context.Context, repoURL, branch string, visit func(path, tip string) error) (bool, error) {
	if repoURL == "" || visit == nil {
		return false, fmt.Errorf("worktree: run-branch checkout requires repository identity and visitor")
	}
	if err := validRunBranch(branch); err != nil {
		return false, err
	}
	key := repoKey(repoURL)
	lock := m.lockFor(key)
	lock.Lock()
	defer lock.Unlock()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	dir := m.repoDirForKey(key)
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
		// No managed mirror for this repository: nothing was ever captured
		// here, so there is no local branch to rescue.
		return false, nil
	}
	tip, err := runBranchTip(ctx, dir, branch)
	if err != nil || tip == "" {
		return false, err
	}
	path := filepath.Join(m.Root, key, terminalCaptureDirectory)
	if err := clearTerminalCapture(ctx, dir, path); err != nil {
		return true, err
	}
	if err := runGit(ctx, dir, "worktree", "add", "--detach", "--", path, tip); err != nil {
		return true, fmt.Errorf("worktree: check out run branch %q for terminal recovery: %w", branch, err)
	}
	visitErr := visit(path, tip)
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
