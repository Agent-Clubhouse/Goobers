package readservice

import (
	"slices"
	"time"

	"github.com/goobers/goobers/api/validate"
)

// DefinitionReloadStatus describes the last background config observation,
// never a filesystem scan performed by a health request.
type DefinitionReloadStatus struct {
	AppliedDigest     string                  `json:"appliedDigest"`
	ObservedDigest    string                  `json:"observedDigest"`
	ObservedAt        time.Time               `json:"observedAt"`
	Watching          bool                    `json:"watching"`
	State             string                  `json:"state"`
	RejectionReason   string                  `json:"rejectionReason,omitempty"`
	CandidateWarnings []validate.CodedWarning `json:"candidateWarnings,omitempty"`
}

// PublishDefinitionReload replaces a fixed-size snapshot. It is safe while
// concurrent health readers are active; no history accumulates here.
func (s *Local) PublishDefinitionReload(status DefinitionReloadStatus) {
	status.CandidateWarnings = cloneReloadWarnings(status.CandidateWarnings)
	s.definitionReload.Store(&status)
}

func (s *Local) definitionReloadSnapshot() *DefinitionReloadStatus {
	stored := s.definitionReload.Load()
	if stored == nil {
		return nil
	}
	copy := *stored
	copy.CandidateWarnings = cloneReloadWarnings(copy.CandidateWarnings)
	return &copy
}

func cloneReloadWarnings(warnings []validate.CodedWarning) []validate.CodedWarning {
	result := slices.Clone(warnings)
	for i := range result {
		if result[i].Safety != nil {
			details := *result[i].Safety
			details.WitnessPath = slices.Clone(details.WitnessPath)
			result[i].Safety = &details
		}
	}
	return result
}
