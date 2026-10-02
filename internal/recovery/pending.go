package recovery

import "fmt"

// Public overflow-pending protocol code, header, and daemon promotion states.
const (
	OverflowPendingCode  = "recovery_overflow_pending"
	PromotionStateHeader = "X-Goobers-Recovery-Promotion-State"
	PromotionAwaiting    = "awaiting-promotion"
	PromotionCapacity    = "waiting-for-capacity"
	PromotionDisabled    = "retention-disabled"
	PromotionDryRun      = "dry-run"
)

// OverflowPendingError distinguishes retained work awaiting a bundle from
// absent or unauthorized recovery. PromotionState is a point-in-time daemon
// observation, not a promise that promotion will succeed on the next sweep.
type OverflowPendingError struct{ PromotionState string }

func (e *OverflowPendingError) Error() string {
	return fmt.Sprintf("recovery snapshot is overflow-pending (daemon promotion state: %s); retry after promotion to the bundle inventory", e.PromotionState)
}

// PendingPromotion reports the constraints used by the daemon's retention
// sweep. Explicit dry-run takes precedence over disabled automatic retention.
func PendingPromotion(enabled, dryRun bool, freeSlots int) *OverflowPendingError {
	state := PromotionAwaiting
	switch {
	case dryRun:
		state = PromotionDryRun
	case !enabled:
		state = PromotionDisabled
	case freeSlots <= 0:
		state = PromotionCapacity
	}
	return &OverflowPendingError{PromotionState: state}
}

// ParsePromotionState accepts only public protocol states; remote error text
// and unknown header values must never become a CLI diagnostic.
func ParsePromotionState(state string) (*OverflowPendingError, bool) {
	switch state {
	case PromotionAwaiting, PromotionCapacity, PromotionDisabled, PromotionDryRun:
		return &OverflowPendingError{PromotionState: state}, true
	default:
		return nil, false
	}
}
