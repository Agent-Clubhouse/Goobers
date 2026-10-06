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
	Unreadable int `json:"unreadable"`
	// Overflow counts snapshots held at the ref tier because the inventory
	// was full when they were captured (#5370). They are NOT part of Used:
	// they occupy no slot. A non-zero Overflow is what "exhausted" now means
	// operationally — cleanup succeeded, so nothing is wedged, but this much
	// work is one durability tier below a bundle until promotion catches up.
	Overflow         int `json:"overflow"`
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
	// ReclaimCandidates are the oldest slot-holding snapshots an operator can
	// act on (terminal owning run), each carrying the exact commands to
	// inspect, restore or abandon it. Populated only for warning and
	// exhausted readings and bounded by RecoveryReclaimCandidateLimit.
	// Commands, not controls: abandoning discards work and stays a deliberate
	// operator decision made at a terminal.
	ReclaimCandidates []RecoveryReclaimCandidate `json:"reclaimCandidates,omitempty"`
	// ReclaimCandidatesTotal counts every actionable snapshot, of which
	// ReclaimCandidates is the oldest bounded prefix.
	ReclaimCandidatesTotal int `json:"reclaimCandidatesTotal,omitempty"`
	// ReclaimHold explains why retention is not currently retiring
	// abandoned or expired snapshots, when it is not. An abandoned snapshot
	// frees its slot only when a retention pass deletes it.
	ReclaimHold *RecoveryReclaimHold `json:"reclaimHold,omitempty"`
	// StatusCommand lists every retained snapshot per run, for an elevated
	// reading whose slots no command above can free.
	StatusCommand string `json:"statusCommand,omitempty"`
}

// RecoveryReclaimCandidateLimit bounds ReclaimCandidates so the read model
// stays small on an inventory holding thousands of entries.
const RecoveryReclaimCandidateLimit = 10

// RecoveryReclaimCandidate is one bundled snapshot occupying an inventory slot
// whose owning run is terminal, so recovery-abandon and recovery-restore both
// accept it.
type RecoveryReclaimCandidate struct {
	RunID         string    `json:"runId"`
	Phase         string    `json:"phase"`
	Ref           string    `json:"ref"`
	PatchDigest   string    `json:"patchDigest"`
	RepositoryKey string    `json:"repositoryKey"`
	CreatedAt     time.Time `json:"createdAt"`
	RetainUntil   time.Time `json:"retainUntil"`
	// Abandoned means an operator already abandoned this exact snapshot; it
	// is waiting for a retention pass to delete it, so no abandon command is
	// offered again.
	Abandoned      bool   `json:"abandoned,omitempty"`
	InspectCommand string `json:"inspectCommand"`
	// RestoreCommand is omitted once RetainUntil has passed: restore refuses
	// expired records.
	RestoreCommand string `json:"restoreCommand,omitempty"`
	AbandonCommand string `json:"abandonCommand,omitempty"`
}

// Retention hold reasons. Each one stops a retention pass from deleting an
// abandoned or expired snapshot, so abandoning frees nothing until it clears.
const (
	RecoveryReclaimHoldDisabled = "disabled"
	RecoveryReclaimHoldDryRun   = "dry-run"
	RecoveryReclaimHoldGrace    = "grace"
)

// RecoveryReclaimHold names the retention setting holding reclamation and the
// change in instance.yaml that releases it.
type RecoveryReclaimHold struct {
	Reason string `json:"reason"`
	// Until is when the first-enable grace window ends. Absent for a grace
	// window that has not started: it starts at the first pass that finds a
	// candidate and then holds deletion for a week.
	Until      *time.Time `json:"until,omitempty"`
	Setting    string     `json:"setting"`
	ConfigFile string     `json:"configFile,omitempty"`
}

// ClassifyRecoveryInventory names the occupancy state for used slots against
// an effective limit. A non-positive limit is unclassifiable rather than
// silently healthy.
//
// Any work held at the overflow tier is exhaustion, whatever the slot count
// currently says (#5370). Occupancy can fall back below the cap the moment a
// slot frees while entries are still waiting at the ref tier, and reporting
// "healthy" there would retract the alarm before the condition it named had
// cleared. The alarm therefore re-arms only once overflow returns to zero.
func ClassifyRecoveryInventory(used, limit, overflow int) string {
	if limit <= 0 {
		return RecoveryInventoryUnavailable
	}
	if overflow > 0 || used >= limit {
		return RecoveryInventoryExhausted
	}
	if used*100 >= limit*RecoveryInventoryHighWaterPercent {
		return RecoveryInventoryWarning
	}
	return RecoveryInventoryHealthy
}
