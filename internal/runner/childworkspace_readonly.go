package runner

import (
	"context"
	"fmt"

	"github.com/goobers/goobers/internal/worktree"
)

func (r *Runner) createChildReadOnlyStage(ctx context.Context, in StartInput, stage string, syncBase bool, branch string) (*stageWorkspace, error) {
	if in.childWorkspace == nil || syncBase || in.workspaceRevision != nil {
		return nil, fmt.Errorf("child read-only stage cannot change its admitted source")
	}
	state := in.childWorkspace
	state.stage.RLock()
	view, err := r.prepareChildReadOnlyView(ctx, in, stage, branch)
	if err != nil {
		state.stage.RUnlock()
		return nil, err
	}
	opts, err := r.childWorkspaceOptions(in)
	if err != nil {
		state.stage.RUnlock()
		return nil, err
	}
	validate := func(ctx context.Context) error {
		_, err := r.cfg.Worktrees.AdoptChildReadOnlyView(ctx, opts, stage)
		return err
	}
	cleanup := func(ctx context.Context) error {
		verified, err := r.cfg.Worktrees.AdoptChildReadOnlyView(ctx, opts, stage)
		if err != nil {
			return err
		}
		return verified.Remove(ctx, worktree.RemoveOptions{})
	}
	return &stageWorkspace{path: view.Path, worktree: view, retainedChild: cleanup, validateReadOnly: validate, release: state.stage.RUnlock}, nil
}

func (r *Runner) prepareChildReadOnlyView(ctx context.Context, in StartInput, stage, branch string) (*worktree.Worktree, error) {
	opts, err := r.childWorkspaceOptions(in)
	if err != nil {
		return nil, err
	}
	primary, err := r.cfg.Worktrees.AdoptChildFromSnapshot(ctx, opts)
	if err != nil {
		return nil, err
	}
	if primary.Path != in.childWorkspace.worktree.Path || (branch != "" && branch != primary.Branch) {
		return nil, fmt.Errorf("child read-only stage changed its primary workspace custody")
	}
	return r.cfg.Worktrees.CreateChildReadOnlyView(ctx, opts, stage)
}
