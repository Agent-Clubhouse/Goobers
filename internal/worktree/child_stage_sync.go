package worktree

import (
	"context"
	"errors"
	"fmt"
)

func (wt *Worktree) mergeStageBase(ctx context.Context, base string) error {
	args := []string{"merge", "--ff", "--no-edit", base}
	var mergeErr error
	if wt.partialMirror {
		mergeErr = wt.manager.runRemoteGit(ctx, wt.repoURL, wt.Path, args...)
	} else {
		mergeErr = runGit(ctx, wt.Path, args...)
	}
	if mergeErr == nil {
		return nil
	}
	resolved, resolveErr := resolveBaseSyncConflict(ctx, wt.Path)
	if resolved {
		return nil
	}
	conflicting, inspectErr := mergeConflictFiles(ctx, wt.Path)
	cleanupErr := runGit(context.WithoutCancel(ctx), wt.Path, "merge", "--abort")
	return baseSyncFailure(CreateOptions{RunID: wt.RunID, BaseRef: base, Branch: wt.Branch}, errors.Join(mergeErr, resolveErr), conflicting, inspectErr, cleanupErr)
}

// syncStageBase preserves dirty work and uses the manager credential/gate seam.
func (wt *Worktree) syncStageBase(ctx context.Context, repository, base string) error {
	if base == "" {
		return fmt.Errorf("worktree: base synchronization requires a base ref")
	}
	status, err := gitOutput(ctx, wt.Path, "status", "--porcelain", "--untracked-files=normal")
	if err != nil {
		return err
	}
	if status != "" {
		return fmt.Errorf("worktree: commit or resolve retained workspace edits before synchronizing base")
	}
	if err := wt.manager.fetchMirror(ctx, wt.repoURL, repository, true); err != nil {
		return err
	}
	return wt.mergeStageBase(ctx, base)
}
