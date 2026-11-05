package workspaceauthority

import (
	"errors"

	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/worktree"
)

// ParentForkRootReadyKind acknowledges the original root checkout. Branch fork
// receipts remain restricted to positive branch IDs; this is a separate role.
const ParentForkRootReadyKind = "isolated.parent.fork.root.ready"

// Workspace resolves a reserved role: zero is the optional original root and
// positive IDs name declaration-ordered branch forks.
func (p ParentForkPlan) Workspace(branch int) (worktree.StageCustody, bool) {
	if branch == 0 && p.Root != nil {
		return *p.Root, true
	}
	if branch <= 0 || branch > len(p.Workspaces) {
		return worktree.StageCustody{}, false
	}
	return p.Workspaces[branch-1], true
}

func validateForkRoot(plan ParentForkPlan) error {
	if plan.Root == nil {
		return nil
	}
	root := *plan.Root
	if root.OwnerRunID != plan.RunID || root.WorkspaceID == "" || root.Branch == "" || root.StartRef == "" || !blobstore.ValidDigest("sha256:"+root.RepositoryDigest) {
		return errors.New("parallel root reservation has invalid ownership")
	}
	for _, branch := range plan.Workspaces {
		if branch.WorkspaceID == root.WorkspaceID || branch.Branch == root.Branch {
			return errors.New("parallel root shares a branch fork workspace")
		}
	}
	return nil
}

func validForkReceiptRole(event journal.Event) bool {
	if event.Runner["kind"] == ParentForkRootReadyKind {
		return event.Branch == 0
	}
	return event.Branch > 0 && event.Branch <= 128
}

// A reservation owns cleanup before the physical hold/ready transition finishes.
// In particular, a terminal sweep cannot delete an active root in that window.
func refusePendingForkCleanup(reader *journal.Reader, events []journal.Event, target worktree.CleanupTarget) error {
	states, err := ReadParentForkStates(reader, events)
	if err != nil {
		return err
	}
	for _, state := range states {
		for branch := 0; branch <= len(state.Plan.Workspaces); branch++ {
			owner, exists := state.Plan.Workspace(branch)
			if !exists || owner.WorkspaceID != target.WorktreeID {
				continue
			}
			if owner.OwnerRunID != target.OwnerRunID || owner.RepositoryDigest != target.RepositoryDigest || owner.StartRef != target.StartRef || target.Pinned {
				return errors.New("reserved parent cleanup identity changed")
			}
			if !state.Ready[branch] {
				return ErrParentReturnPending
			}
		}
	}
	return nil
}

func validateParentForkOwner(plan ParentForkPlan, id journal.RunIdentity) error {
	if plan.RunID != id.RunID || plan.Gaggle != id.Gaggle || id.Child != nil {
		return errors.New("parallel fork plan owner changed")
	}
	return validateForkRoot(plan)
}

// AllWorkspaces includes the root when determining capacity and archive custody.
func (p ParentForkPlan) AllWorkspaces() []worktree.StageCustody {
	result := append([]worktree.StageCustody(nil), p.Workspaces...)
	if p.Root != nil {
		result = append(result, *p.Root)
	}
	return result
}
