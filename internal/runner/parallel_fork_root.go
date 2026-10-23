package runner

import (
	"context"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workspaceauthority"
)

// ParentForkRootReadyKind uses the shared workspaceauthority.ParentForkRootReadyKind contract.
const ParentForkRootReadyKind = workspaceauthority.ParentForkRootReadyKind

func (r *Runner) prepareParallelRoot(ctx context.Context, run *journal.Run, url string, state *parentForkState) error {
	if state.Plan.Root == nil {
		return nil
	}
	owner := *state.Plan.Root
	if state.Ready[0] {
		_, err := r.cfg.Worktrees.AdoptHeldStage(ctx, url, owner)
		return err
	}
	if _, err := r.cfg.Worktrees.HoldReservedStage(ctx, url, owner); err != nil {
		return err
	}
	return RecordParentForkReady(run, state.Plan.Sequence, state.Reference, 0, owner)
}
