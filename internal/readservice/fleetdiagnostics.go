package readservice

import "context"

// FleetDiagnosticsAvailable reports whether bounded fleet polling can use the
// indexed run model. Diagnostics must not fall back to exhaustive historical
// journal scans when that model is disabled or unavailable.
func (s *Local) FleetDiagnosticsAvailable(ctx context.Context) bool {
	if !s.readModelReads || s.sources.ReadModel == nil {
		return false
	}
	state, err := s.sources.ReadModel.State(ctx)
	return err == nil && state.Ready
}
