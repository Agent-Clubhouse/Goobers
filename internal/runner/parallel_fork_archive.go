package runner

import (
	"errors"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workspaceauthority"
)

// ParentForkCustody uses the shared workspaceauthority.ParentForkCustody contract.
type ParentForkCustody = workspaceauthority.ParentForkCustody

func validateParentArchiveSource(value ParentWorkspaceArchive) error {
	return workspaceauthority.ValidateParentArchiveSource(value)
}

// ParentForkArchivePlan verifies the exact host plan, branch-ready receipt and
// latest workspace source before the daemon derives archive policy or restores.
func ParentForkArchivePlan(reader *journal.Reader, archive ParentWorkspaceArchive) (ParentForkPlan, int, error) {
	var empty ParentForkPlan
	if archive.Fork == nil || validateParentArchiveSource(archive) != nil {
		return empty, 0, errors.New("fork archive lacks its exact host source")
	}
	events, err := reader.Events()
	if err != nil {
		return empty, 0, err
	}
	states, err := readParentForkStates(reader, events)
	if err != nil {
		return empty, 0, err
	}
	fork := states[archive.Fork.Sequence]
	if fork == nil || fork.Reference != archive.Fork.Plan || fork.PlannedAt != archive.HoldSeq {
		return empty, 0, errors.New("fork archive plan changed")
	}
	state, err := readParentWorkspaceState(events, archive.Custody.Workspace.OwnerRunID, archive.Custody.Workspace.Branch)
	if err != nil {
		return empty, 0, err
	}
	owner, exists := fork.Plan.Workspace(state.Branch)
	if !state.MatchesArchiveSource(archive) || state.ReturnedAt != archive.ReturnSeq || !exists || !fork.Ready[state.Branch] || owner != archive.Custody.Workspace {
		return empty, 0, errors.New("fork archive is not the current acknowledged workspace")
	}
	return fork.Plan, state.Branch, nil
}
