package runner

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
	branches, err := parentArchiveBranches(events)
	if err != nil {
		return empty, false, err
	}
	for _, branch := range branches {
		state, err := readParentWorkspaceState(events, id.RunID, branch.name)
		if err != nil {
			return empty, false, err
		}
		if state.retiredAt == 0 || state.archive.Custody.Workspace.WorkspaceID != target.WorktreeID {
			continue
		}
		custody := state.archive.Custody.Workspace
		if target.Pinned || custody.OwnerRunID != target.OwnerRunID || custody.RepositoryDigest != target.RepositoryDigest || custody.StartRef != target.StartRef {
			return empty, false, errors.New("parent archive cleanup identity changed")
		}
		return state.archive, true, nil
	}
	return empty, false, nil
}
