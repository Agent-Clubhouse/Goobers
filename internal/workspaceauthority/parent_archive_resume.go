package workspaceauthority

import (
	"errors"

	"github.com/goobers/goobers/internal/journal"
)

// ParentArchiveBranch identifies a branch with a retained workspace archive.
type ParentArchiveBranch struct {
	Name  string
	Index int
}

// ParentArchiveBranches lists bounded retirement branches with consistent journal attribution.
func ParentArchiveBranches(events []journal.Event) ([]ParentArchiveBranch, error) {
	var branches []ParentArchiveBranch
	seen := map[string]int{}
	for _, event := range events {
		if event.Type != journal.EventRunnerAnnotation || event.Runner["kind"] != ParentContributionRetiredKind {
			continue
		}
		value, err := DecodeParentWorkspaceArchive(event)
		if err != nil {
			return nil, err
		}
		name := value.Custody.Workspace.Branch
		if prior, exists := seen[name]; exists {
			if prior != event.Branch {
				return nil, errors.New("parent archive branch attribution changed")
			}
			continue
		}
		if len(branches) >= 128 || event.Branch < 0 || event.Branch > 128 {
			return nil, errors.New("parent archive branch limit exceeded")
		}
		seen[name] = event.Branch
		branches = append(branches, ParentArchiveBranch{Name: name, Index: event.Branch})
	}
	return branches, nil
}
