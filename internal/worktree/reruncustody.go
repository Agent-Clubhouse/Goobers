package worktree

import (
	"context"
	"fmt"
	"path/filepath"
)

// VerifyRunBranch checks existing managed custody without fetching or creating
// a repository or branch. Recovery archives must be restored by the authorized
// recovery path before a stage rerun can use them.
func (m *Manager) VerifyRunBranch(ctx context.Context, repoURL, branch string, pinned bool) error {
	key := repoKey(repoURL)
	held := m.lockFor(key)
	if err := lockWithContext(ctx, held); err != nil {
		return err
	}
	defer held.Unlock()
	foundBranch := false
	_, err := m.withRecoveryRepositoriesLocked(ctx, key, repoURL, func(repositories []string) error {
		for _, repository := range repositories {
			if (filepath.Base(repository) == "pin") == pinned && branchExists(ctx, repository, branch) {
				foundBranch = true
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !foundBranch {
		return fmt.Errorf("preserved run branch %q is unavailable; restore authorized recovery custody before rerunning", branch)
	}
	return nil
}
