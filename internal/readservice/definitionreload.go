package readservice

import "time"

// DefinitionReloadStatus describes the last background config observation,
// never a filesystem scan performed by a health request.
type DefinitionReloadStatus struct {
	AppliedDigest  string    `json:"appliedDigest"`
	ObservedDigest string    `json:"observedDigest"`
	ObservedAt     time.Time `json:"observedAt"`
	Watching       bool      `json:"watching"`
	State          string    `json:"state"`
}

// PublishDefinitionReload replaces a fixed-size snapshot. It is safe while
// concurrent health readers are active; no history accumulates here.
func (s *Local) PublishDefinitionReload(status DefinitionReloadStatus) {
	s.definitionReload.Store(&status)
}

func (s *Local) definitionReloadSnapshot() *DefinitionReloadStatus {
	stored := s.definitionReload.Load()
	if stored == nil {
		return nil
	}
	copy := *stored
	return &copy
}
