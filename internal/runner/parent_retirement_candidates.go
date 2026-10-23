package runner

import (
	"errors"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workspaceauthority"
)

// ParentRetirementCandidate identifies a host-owned physical workstream.
// RetirementSeq is zero until archived; Workspace.Archive is then unset too.
type ParentRetirementCandidate struct {
	Workspace     ParentWorkspaceArchive
	Branch        int
	RetirementSeq uint64
}

// ParentRetirementCandidates is bounded by the physical parent branch limit.
// An unacknowledged newer hold fails closed, never selecting an older return.
func ParentRetirementCandidates(reader *journal.Reader) ([]ParentRetirementCandidate, error) {
	id, err := reader.Identity()
	if err != nil {
		return nil, err
	}
	if id.Child != nil {
		return nil, nil
	}
	events, err := reader.Events()
	if err != nil {
		return nil, err
	}
	if err := requireParallelForkArchiveOwnership(reader, events); err != nil {
		return nil, err
	}
	branches, err := heldParentBranches(events)
	if err != nil {
		return nil, err
	}
	var result []ParentRetirementCandidate
	for _, branch := range branches {
		state, err := readParentWorkspaceState(events, id.RunID, branch.Name)
		if err != nil {
			return nil, err
		}
		value := state.Archive
		if state.RetiredAt == 0 {
			value = state.ArchiveValue(journal.Ref{})
		}
		result = append(result, ParentRetirementCandidate{Workspace: value, Branch: branch.Index, RetirementSeq: state.RetiredAt})
	}
	return result, nil
}

func heldParentBranches(events []journal.Event) ([]parentArchiveBranch, error) {
	var result []parentArchiveBranch
	seen := map[string]int{}
	for _, event := range events {
		custody, relevant, err := parentEventCustody(event)
		if err != nil {
			return nil, err
		}
		if !relevant {
			continue
		}
		name := custody.Workspace.Branch
		if prior, ok := seen[name]; ok {
			if prior != event.Branch {
				return nil, errors.New("parent retirement branch attribution changed")
			}
			continue
		}
		if len(result) >= 128 || event.Branch < 0 || event.Branch > 128 {
			return nil, errors.New("parent retirement branch limit exceeded")
		}
		seen[name] = event.Branch
		result = append(result, parentArchiveBranch{Name: name, Index: event.Branch})
	}
	return result, nil
}

func parentEventCustody(event journal.Event) (ContainedParentWorkspaceCustody, bool, error) {
	return workspaceauthority.ParentEventCustody(event)
}
