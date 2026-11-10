package workspaceauthority

import (
	"errors"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/worktree"
)

// ParentCleanupArchive selects removal authority only for the exact current
// retired checkout. A restored receipt removes this authority; subsequent work
// must be captured and retired again. The host still verifies archive custody
// and the live checkout before allowing deletion.
func ParentCleanupArchive(reader *journal.Reader, target worktree.CleanupTarget) (ParentWorkspaceArchive, bool, error) {
	var empty ParentWorkspaceArchive
	id, err := reader.Identity()
	if err != nil {
		return empty, false, err
	}
	if id.RunID != target.OwnerRunID {
		return empty, false, errors.New("parent archive cleanup owner mismatch")
	}
	if id.Child != nil {
		return empty, false, nil
	}
	events, err := reader.Events()
	if err != nil {
		return empty, false, err
	}
	branches, err := ParentArchiveBranches(events)
	if err != nil {
		return empty, false, err
	}
	for _, branch := range branches {
		state, err := ReadParentWorkspaceState(events, id.RunID, branch.Name)
		if err != nil {
			return empty, false, err
		}
		if state.RetiredAt == 0 || state.Archive.Custody.Workspace.WorkspaceID != target.WorktreeID {
			continue
		}
		custody := state.Archive.Custody.Workspace
		if target.Pinned || custody.OwnerRunID != target.OwnerRunID || custody.RepositoryDigest != target.RepositoryDigest || custody.StartRef != target.StartRef {
			return empty, false, errors.New("parent archive cleanup identity changed")
		}
		return state.Archive, true, nil
	}
	return empty, false, nil
}
