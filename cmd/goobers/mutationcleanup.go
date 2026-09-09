package main

import (
	"context"
	"path/filepath"

	"github.com/goobers/goobers/internal/mutationsidecar"
	"github.com/goobers/goobers/internal/worktree"
)

// mutationCleanupGuard binds recovery to the host-selected run root. A receipt
// cannot choose its destination via claimRunId or a path in its payload.
func mutationCleanupGuard(runsDir string) worktree.ManagerOption {
	return worktree.WithMutationReceiptCleanup(func(ctx context.Context, target worktree.CleanupTarget) error {
		return mutationsidecar.RecoverBeforeCleanup(ctx, target.Path, target.WorktreeID, target.OwnerRunID, filepath.Join(runsDir, target.OwnerRunID))
	})
}
