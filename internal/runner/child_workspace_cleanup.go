package runner

import (
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/worktree"
)

// VerifyChildWorkspaceCleanup prevents startup reaping from destroying the
// exact view needed to reconcile a stopped worker. Completed sibling views
// may retire independently; the shared child fork requires the whole family.
func VerifyChildWorkspaceCleanup(reader *journal.Reader, id journal.RunIdentity, target worktree.CleanupTarget) error {
	if id.Child == nil {
		return nil
	}
	admission, err := PinnedChildWorkspaceAdmission(reader, id)
	if err != nil {
		return err
	}
	if admission == nil || target.OwnerRunID != id.RunID || target.Gaggle != id.Gaggle || target.RepositoryDigest != admission.RepositoryDigest {
		return invoke.ErrWorkspaceNotQuiescent
	}
	events, err := reader.Events()
	if err != nil {
		return err
	}
	pending, err := pendingChildWorkspaceWriters(reader, id, events)
	if err != nil {
		return err
	}
	if target.WorktreeID == admission.WorkspaceID {
		if len(pending) != 0 {
			return invoke.ErrWorkspaceNotQuiescent
		}
		return nil
	}
	// A detached view is named from the pinned child workspace and exact stage.
	// Journal status alone never substitutes for that stage's writer join.
	for _, event := range events {
		if event.Type != journal.EventStageStarted && event.Type != journal.EventReviewerStarted {
			continue
		}
		viewID, err := worktree.ChildReadOnlyViewID(id.Gaggle, admission.WorkspaceID, event.Stage)
		if err != nil || viewID != target.WorktreeID {
			continue
		}
		for _, scope := range pending {
			if scope.Stage == id.RunID+":"+event.Stage || scope.Branch == 0 {
				return invoke.ErrWorkspaceNotQuiescent
			}
		}
		return nil
	}
	return invoke.ErrWorkspaceNotQuiescent
}
