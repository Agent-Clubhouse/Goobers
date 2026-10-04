package interactiveaccess

import (
	"slices"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// SetSourceProposalAvailable advertises an installed proposal service, not a grant.
func (s *Service) SetSourceProposalAvailable(available bool) {
	s.sourceProposalAvailable.Store(available)
}

func workbenchProposableSource(g *apiv1.Gaggle) bool {
	if g.Spec.Workbench == nil {
		return false
	}
	for _, source := range g.Spec.Workbench.Sources {
		if (source.Kind != "documents" && source.Kind != "relationships") || source.Writes == nil {
			continue
		}
		if (source.Kind == "documents" && slices.Contains(source.Writes.Metadata, "assign-objective")) || (source.Kind == "relationships" && slices.Contains(source.Writes.Metadata, "aliases")) {
			return true
		}
		if slices.Contains(source.Writes.Fields, "title") || slices.Contains(source.Writes.Fields, "description") || slices.Contains(source.Writes.Relationships, "references") || slices.Contains(source.Writes.Relationships, "contributes-to") {
			return true
		}
	}
	return false
}
