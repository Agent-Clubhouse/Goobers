package runner

import (
	"context"
	"errors"
	"fmt"

	"github.com/goobers/goobers/internal/worktree"
)

func (r *Runner) createReadOnlyWorkspace(ctx context.Context, in StartInput, stage string, syncBase bool, branch string) (*stageWorkspace, error) {
	if in.workspaceRevision != nil {
		return r.createRevisionWorkspace(ctx, in, stage, syncBase, branch)
	}
	if syncBase {
		return nil, fmt.Errorf("create read-only workspace: syncBase requires a writable repo workspace")
	}
	if branch != "" {
		return nil, fmt.Errorf("create read-only workspace: a rebound branch requires a writable repo workspace")
	}
	if in.pinnedWorkspace != nil {
		return r.createPinnedInspectionWorkspace(ctx, in, stage)
	}
	repoURL, err := r.cfg.RepoCloneURL(in.RepoRef)
	if err != nil {
		return nil, err
	}
	baseRef := in.RepoRef.Branch
	if baseRef == "" {
		baseRef = "main"
	}
	sparse := sparseCones(in.RepoRef.Checkout)
	wt, err := r.cfg.Worktrees.Create(ctx, worktree.CreateOptions{
		RepoURL:    repoURL,
		RunID:      in.RunID + "-" + stage,
		OwnerRunID: in.RunID,
		BaseRef:    baseRef,
		// Detached checkouts let read-only stages coexist within one run.
		Branch: "",
		Sparse: sparse,
	})
	if err != nil {
		return nil, fmt.Errorf("create read-only worktree: %w", err)
	}
	additional, err := r.provisionAdditionalCheckouts(ctx, in, stage)
	if err != nil {
		_ = wt.Remove(ctx, worktree.RemoveOptions{})
		return nil, err
	}
	return &stageWorkspace{path: wt.Path, worktree: wt, additional: additional, sparse: sparse}, nil
}

func (r *Runner) createPinnedInspectionWorkspace(ctx context.Context, in StartInput, stage string) (*stageWorkspace, error) {
	in.pinnedStage.Lock()
	if err := r.preparePinnedStage(ctx, in, false, ""); err != nil {
		in.pinnedStage.Unlock()
		return nil, err
	}
	sha, err := in.pinnedWorkspace.PreparePinnedInspection(ctx, sparseCones(in.RepoRef.Checkout))
	if err != nil {
		in.pinnedStage.Unlock()
		return nil, err
	}
	additional, err := r.provisionAdditionalCheckouts(ctx, in, stage)
	if err != nil {
		err = errors.Join(err, in.pinnedWorkspace.ResetPinnedRevision(ctx, sha))
		in.pinnedStage.Unlock()
		return nil, err
	}
	return &stageWorkspace{
		path: in.pinnedWorkspace.Path, worktree: in.pinnedWorkspace,
		additional: additional, release: in.pinnedStage.Unlock,
		reset: func(ctx context.Context) error { return in.pinnedWorkspace.ResetPinnedRevision(ctx, sha) },
	}, nil
}
