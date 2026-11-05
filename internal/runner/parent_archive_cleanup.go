package runner

import (
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workspaceauthority"
	"github.com/goobers/goobers/internal/worktree"
)

// ParentCleanupArchive uses the shared workspaceauthority.ParentCleanupArchive contract.
func ParentCleanupArchive(reader *journal.Reader, target worktree.CleanupTarget) (ParentWorkspaceArchive, bool, error) {
	return workspaceauthority.ParentCleanupArchive(reader, target)
}
