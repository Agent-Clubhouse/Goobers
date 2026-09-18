package readservice

import "time"

// Recovery-inventory occupancy states (#5343, #4911 AC5).
//
// The recovery inventory is ONE directory shared by every gaggle on the
// instance, and a full one does not degrade execution — it stops it. A
// worktree cannot be cleaned without a durable recovery handoff, a worktree
// that cannot be cleaned cannot be reused, and stages then fail at
// `create worktree` with an error naming neither recovery nor the run that
// filled the inventory (#5354). Occupancy therefore has to be readable
// BEFORE it fails a stage, not reconstructed afterwards from the failure.
const (
	// RecoveryInventoryHealthy means occupancy is below the high-water mark.
	RecoveryInventoryHealthy = "healthy"
	// RecoveryInventoryWarning means occupancy has reached the high-water
	// mark but slots remain. Cleanup still succeeds; the instance is on a
	// path to the exhausted state and an operator can still act cheaply.
	RecoveryInventoryWarning = "warning"
	// RecoveryInventoryExhausted means every configured slot is occupied.
	// Durable handoffs, worktree cleanup and unrelated runs fail now.
	RecoveryInventoryExhausted = "exhausted"
	// RecoveryInventoryUnavailable means occupancy could not be measured.
	// It is not a healthy reading: an unreadable inventory must never be
	// reported as an empty one.
	RecoveryInventoryUnavailable = "unavailable"
)

// RecoveryInventoryHighWaterPercent is the documented high-water threshold:
// occupancy at or above 80% of the effective limit is reported as warning.
// It is a percentage of the operator's own configured cap rather than an
// absolute slot count, so raising retention.recovery.maxSnapshots moves the
// warning with it instead of leaving a cap-sized instance permanently warned.
const RecoveryInventoryHighWaterPercent = 80

// RecoveryInventoryStatus is recovery-inventory occupancy as the read model
// reports it.
//
// Used counts every reservation directory occupying a slot, including the
// Unreadable ones: incomplete reservations (a crashed publish leaves a
// directory holding only lock files) count against the cap until they are
// reconciled, so a Used that excluded them would disagree with the number the
// next refused cleanup reports (#5177).
type RecoveryInventoryStatus struct {
	State string `json:"state"`
	// Used and Limit are the same two numbers a refusal reports, resolved
	// through the same policy resolution every writer into the inventory
	// uses, so they cannot be two different numbers for one directory.
	Used  int `json:"used"`
	Limit int `json:"limit"`
	// Unreadable is the subset of Used that holds no interpretable record.
	Unreadable       int `json:"unreadable"`
	HighWaterPercent int `json:"highWaterPercent"`
	// EarliestRetainUntil is when the next slot can be reclaimed by policy.
	// Absent when the inventory is empty or no deadline could be read: it is
	// what distinguishes ordinary pressure from an inventory wedged behind a
	// retain floor no eviction can shorten.
	EarliestRetainUntil *time.Time `json:"earliestRetainUntil,omitempty"`
	InventoryRoot       string     `json:"inventoryRoot,omitempty"`
	// PolicySource names how the limit was resolved, so a fallback to a
	// carried or built-in value is reportable rather than silent (#5092).
	PolicySource string `json:"policySource,omitempty"`
	// Error explains an unavailable reading. An operator reading only
	// "unavailable" cannot tell a locked inventory from a corrupt record.
	Error      string    `json:"error,omitempty"`
	ObservedAt time.Time `json:"observedAt"`
}

// ClassifyRecoveryInventory names the occupancy state for used slots against
// an effective limit. A non-positive limit is unclassifiable rather than
// silently healthy.
func ClassifyRecoveryInventory(used, limit int) string {
	if limit <= 0 {
		return RecoveryInventoryUnavailable
	}
	if used >= limit {
		return RecoveryInventoryExhausted
	}
	if used*100 >= limit*RecoveryInventoryHighWaterPercent {
		return RecoveryInventoryWarning
	}
	return RecoveryInventoryHealthy
}
