package interactivesession

import (
	"bytes"

	"github.com/goobers/goobers/internal/sessioning"
)

// Selection cannot be rewritten by redaction: that would point at a different
// native object. Reject it before acceptance, or omit it from a later safe view.
func (s *Service) safeRepairTarget(target *sessioning.PRRepairTarget) bool {
	raw, err := sessioning.MarshalPRRepairTarget(target)
	return err == nil && bytes.Equal(raw, s.Scrubber.Scrub(raw))
}
