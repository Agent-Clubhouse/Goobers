package livejournal

import (
	"fmt"

	"github.com/goobers/goobers/internal/journal"
)

// validateReservationIdentity runs under the liveRun lock before applying any
// ops. The first header owns run.yaml permanently; a later reservation must
// not silently lend that run's identity to another instance's operations.
// Read the durable header even on the warm-cache path so restart and in-memory
// behavior agree. Headerless stage emissions retain their existing scoped
// protocol; they do not claim or replace a run identity.
func (run *liveRun) validateReservationIdentity(req EmitRequest) error {
	if req.Gaggle != run.gaggle {
		return fmt.Errorf("livejournal: request gaggle %q differs from run gaggle %q", req.Gaggle, run.gaggle)
	}
	if req.Open == nil {
		return nil
	}
	reader, err := journal.OpenReadOnly(run.dir)
	if err != nil {
		return fmt.Errorf("livejournal: read reserved identity: %w", err)
	}
	identity, err := reader.Identity()
	if err != nil {
		return fmt.Errorf("livejournal: read reserved identity: %w", err)
	}
	if req.Open.Identity.RunID != identity.RunID || req.Open.Identity.Gaggle != identity.Gaggle {
		return fmt.Errorf("livejournal: reservation run/gaggle does not match durable identity")
	}
	if req.Open.Identity.InstanceID != identity.InstanceID {
		return fmt.Errorf("livejournal: reservation instance identity %q differs from durable identity %q", req.Open.Identity.InstanceID, identity.InstanceID)
	}
	return nil
}
