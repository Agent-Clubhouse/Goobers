package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// WithExistingBranchCheckout materializes a detached temporary checkout of a
// branch already present in the managed mirror. It never fetches or creates a
// mirror. The checkout is removed after visit returns.
func (m *Manager) WithExistingBranchCheckout(ctx context.Context, repoURL, branch, base string, visit func(string) error) (found bool, retErr error) {
	branch = strings.TrimSpace(branch)
	base = strings.TrimSpace(base)
	if repoURL == "" || branch == "" || base == "" || visit == nil {
		return false, fmt.Errorf("existing branch checkout requires repository identity, branch, base, and visitor")
	}
	key := repoKey(repoURL)
	lock := m.lockFor(key)
	lock.Lock()
	defer lock.Unlock()
	repository := m.repoDirForKey(key)
	if _, err := os.Stat(repository); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("inspect managed mirror for branch checkout: %w", err)
	}
	if !branchExists(ctx, repository, branch) {
		return false, nil
	}
	if _, err := gitOutput(ctx, repository, "rev-parse", "--verify", base+"^{commit}"); err != nil {
		return false, fmt.Errorf("resolve terminal recovery base %q: %w", base, err)
	}
	parent := filepath.Join(m.Root, key, "terminal-recovery")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return false, fmt.Errorf("create terminal recovery checkout root: %w", err)
	}
	path, err := os.MkdirTemp(parent, "checkout-")
	if err != nil {
		return false, fmt.Errorf("create terminal recovery checkout: %w", err)
	}
	if err := os.Remove(path); err != nil {
		return false, fmt.Errorf("prepare terminal recovery checkout: %w", err)
	}
	defer func() {
		cleanupErr := runGit(context.Background(), repository, "worktree", "remove", "--force", path)
		if removeErr := os.RemoveAll(path); cleanupErr == nil {
			cleanupErr = removeErr
		} else if removeErr != nil {
			cleanupErr = errors.Join(cleanupErr, removeErr)
		}
		if cleanupErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("remove terminal recovery checkout: %w", cleanupErr))
		}
	}()
	if err := runGit(ctx, repository, "worktree", "add", "--detach", "--force", path, "refs/heads/"+branch); err != nil {
		return false, fmt.Errorf("materialize terminal recovery branch %q: %w", branch, err)
	}
	return true, visit(path)
}
