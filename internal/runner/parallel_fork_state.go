package runner

import (
	"encoding/json"
	"errors"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workspaceauthority"
	"github.com/goobers/goobers/internal/worktree"
)

// ParentForkPlannedKind uses the shared workspaceauthority.ParentForkPlannedKind contract.
const ParentForkPlannedKind = workspaceauthority.ParentForkPlannedKind

// ParentForkReadyKind uses the shared workspaceauthority.ParentForkReadyKind contract.
const ParentForkReadyKind = workspaceauthority.ParentForkReadyKind
const maxParentForkPlanBytes = workspaceauthority.MaxParentForkPlanBytes
const maxParentForkBundleBytes = workspaceauthority.MaxParentForkBundleBytes

// ParentForkPlan uses the shared workspaceauthority.ParentForkPlan contract.
type ParentForkPlan = workspaceauthority.ParentForkPlan

type parentForkState = workspaceauthority.ParentForkState

func readParentForkStates(reader *journal.Reader, events []journal.Event) (map[uint64]*parentForkState, error) {
	return workspaceauthority.ReadParentForkStates(reader, events)
}

// Reserve all declared forks together under the root execution owner. Existing
// contained-parent holds and earlier visits share the same 128-checkout bound.
func reserveParentForks(events []journal.Event, states map[uint64]*parentForkState, proposed []worktree.StageCustody) error {
	owners := map[string]worktree.StageCustody{}
	add := func(owner worktree.StageCustody) error {
		if previous, exists := owners[owner.WorkspaceID]; exists && previous != owner {
			return errors.New("parallel fork workspace ownership changed")
		}
		if owner.WorkspaceID == "" || owner.Branch == "" {
			return errors.New("invalid parent workspace ownership")
		}
		owners[owner.WorkspaceID] = owner
		if len(owners) > 128 {
			return errors.New("parent workspace fork limit exceeded before fan-out")
		}
		return nil
	}
	for _, event := range events {
		if event.Type != journal.EventRunnerAnnotation || event.Runner["kind"] != ContainedParentWorkspaceKind {
			continue
		}
		var custody ContainedParentWorkspaceCustody
		data, err := json.Marshal(event.Runner["custody"])
		if err != nil || len(data) > 8192 || json.Unmarshal(data, &custody) != nil || custody.Version != 1 || custody.Origin == nil {
			return errors.New("invalid parent custody before fork reservation")
		}
		if err := add(custody.Workspace); err != nil {
			return err
		}
	}
	for _, state := range states {
		for _, owner := range state.Plan.AllWorkspaces() {
			if err := add(owner); err != nil {
				return err
			}
		}
	}
	for _, owner := range proposed {
		if err := add(owner); err != nil {
			return err
		}
	}
	return nil
}

// Every reserved fork must have an acknowledged physical owner. Incomplete
// creation retains the plan and source until recovery can settle that custody.
func requireParallelForkArchiveOwnership(reader *journal.Reader, events []journal.Event) error {
	states, err := readParentForkStates(reader, events)
	if err != nil {
		return err
	}
	for _, fork := range states {
		for branch := 0; branch <= len(fork.Plan.Workspaces); branch++ {
			owner, exists := fork.Plan.Workspace(branch)
			if !exists {
				continue
			}
			if !fork.Ready[branch] {
				return ErrParentReturnPending
			}
			state, err := readParentWorkspaceState(events, owner.OwnerRunID, owner.Branch)
			if err != nil {
				return err
			}
			if state.HeldAt == 0 || state.Hold.Workspace != owner {
				return ErrParentReturnPending
			}
		}
	}
	return nil
}
