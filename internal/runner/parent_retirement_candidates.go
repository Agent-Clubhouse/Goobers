package runner

import (
	"encoding/json"
	"errors"

	"github.com/goobers/goobers/internal/journal"
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
		state, err := readParentWorkspaceState(events, id.RunID, branch.name)
		if err != nil {
			return nil, err
		}
		value := state.archive
		if state.retiredAt == 0 {
			value = ParentWorkspaceArchive{Version: 1, Custody: state.contribution.Custody, ContractDigest: state.contribution.ContractDigest, Output: state.contribution.Output, HoldSeq: state.heldAt, ReturnSeq: state.returnedAt}
		}
		result = append(result, ParentRetirementCandidate{Workspace: value, Branch: branch.index, RetirementSeq: state.retiredAt})
	}
	return result, nil
}

func heldParentBranches(events []journal.Event) ([]parentArchiveBranch, error) {
	var result []parentArchiveBranch
	seen := map[string]int{}
	for _, event := range events {
		if event.Type != journal.EventRunnerAnnotation || event.Runner["kind"] != ContainedParentWorkspaceKind {
			continue
		}
		var custody ContainedParentWorkspaceCustody
		data, err := json.Marshal(event.Runner["custody"])
		if err != nil || len(data) > 8192 || json.Unmarshal(data, &custody) != nil || custody.Workspace.Branch == "" {
			return nil, errors.New("invalid parent retirement custody")
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
		result = append(result, parentArchiveBranch{name: name, index: event.Branch})
	}
	return result, nil
}
