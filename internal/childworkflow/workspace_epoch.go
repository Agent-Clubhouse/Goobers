package childworkflow

import (
	"context"

	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// ExecutionFork selects the immutable starting tree of one execution. Epoch
// zero starts at the accepted parent fork; a human epoch starts at the verified
// preceding result. RetainedFork always remains the accepted parent base used
// to calculate the complete contribution for disposition.
func (c *WorkspaceCoordinator) ExecutionFork(ctx context.Context, child triggerqueue.ChildRecord, runID, repoURL string) (recovery.ChildSnapshot, error) {
	if c == nil || c.Queue == nil {
		return recovery.ChildSnapshot{}, ErrAuthorityUnavailable
	}
	if runID == child.RunID {
		return c.RetainedFork(ctx, child, repoURL)
	}
	epoch, err := c.Queue.ChildExecution(ctx, child.Identity, runID)
	if err != nil {
		return recovery.ChildSnapshot{}, err
	}
	result, err := c.ReadExecutionResult(ctx, child, epoch.SourceRunID, repoURL)
	if err != nil {
		return recovery.ChildSnapshot{}, err
	}
	if epoch.Epoch == 0 || result.ResultRef != epoch.SourceResultRef || result.Snapshot == nil {
		return recovery.ChildSnapshot{}, triggerqueue.ErrChildResultUnavailable
	}
	return *result.Snapshot, nil
}

// PrepareExecution creates the current human epoch's distinct managed fork
// exclusively from the retained source result. It never samples a live source
// or parent checkout. Journal publication must still take the transactional
// WithChildExecutionResume fence before releasing any stage effects.
func (c *WorkspaceCoordinator) PrepareExecution(ctx context.Context, child triggerqueue.ChildRecord, repoURL string) (*runner.ChildWorkspaceAdmission, error) {
	if c == nil || c.Queue == nil || c.Worktrees == nil || repoURL == "" {
		return nil, ErrAuthorityUnavailable
	}
	current, err := c.Queue.GetChild(ctx, child.Identity)
	if err != nil {
		return nil, err
	}
	if current.AcceptanceID != child.AcceptanceID || current.ProposalDigest != child.ProposalDigest || current.RunID != child.RunID || current.ExecutionEpoch == 0 || current.ExecutionEpoch != child.ExecutionEpoch || current.ActiveRunID() != child.ActiveRunID() || current.State != triggerqueue.ChildQueued || current.CancellationRequested || !current.TombstonedAt.IsZero() {
		return nil, ErrAuthorityUnavailable
	}
	if err := c.Queue.CheckChildParentOpen(ctx, child.Identity.ChildParent); err != nil {
		return nil, err
	}
	epoch, err := c.Queue.ChildExecution(ctx, child.Identity, child.ActiveRunID())
	if err != nil {
		return nil, err
	}
	snapshot, err := c.ExecutionFork(ctx, child, child.ActiveRunID(), repoURL)
	if err != nil {
		return nil, err
	}
	stored, err := c.Queue.ChildExecutionResult(ctx, child.Identity, epoch.SourceRunID)
	if err != nil {
		return nil, err
	}
	return c.prepareExecutionFork(ctx, child, repoURL, snapshot, stored.Bundle)
}
