package runner

import (
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workspaceauthority"
	"github.com/goobers/goobers/internal/worktree"
)

// VerifyChildWorkspaceCleanup uses the shared workspaceauthority.VerifyChildWorkspaceCleanup contract.
func VerifyChildWorkspaceCleanup(reader *journal.Reader, id journal.RunIdentity, target worktree.CleanupTarget) error {
	return workspaceauthority.VerifyChildWorkspaceCleanup(reader, id, target)
}
