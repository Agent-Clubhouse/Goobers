package runner

import (
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workspaceauthority"
)

// ParentContributionRestoredKind uses the shared workspaceauthority.ParentContributionRestoredKind contract.
const ParentContributionRestoredKind = workspaceauthority.ParentContributionRestoredKind

// ParentWorkspaceArchive uses the shared workspaceauthority.ParentWorkspaceArchive contract.
type ParentWorkspaceArchive = workspaceauthority.ParentWorkspaceArchive

// ErrParentReturnPending uses the shared workspaceauthority.ErrParentReturnPending contract.
var ErrParentReturnPending = workspaceauthority.ErrParentReturnPending

type parentWorkspaceState = workspaceauthority.ParentWorkspaceState

func readParentWorkspaceState(events []journal.Event, runID, branch string) (parentWorkspaceState, error) {
	return workspaceauthority.ReadParentWorkspaceState(events, runID, branch)
}

func decodeParentWorkspaceArchive(event journal.Event) (ParentWorkspaceArchive, error) {
	return workspaceauthority.DecodeParentWorkspaceArchive(event)
}
