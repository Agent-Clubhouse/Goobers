package localscheduler

import (
	"context"
	"errors"
)

// ErrClaimRead marks unavailable ownership evidence. Unlike a slow provider
// count, it must never synthesize demand or consume a pending schedule fire.
var ErrClaimRead = errors.New("claim ownership read failed")

// SnapshotErr adapts the already-open file ledger to ClaimStore. File reads
// happened in OpenClaimLedger; this is the same in-memory snapshot as before.
func (l *ClaimLedger) SnapshotErr(context.Context) ([]ClaimEntry, error) { return l.Snapshot(), nil }

// HistorySnapshotErr preserves the file ledger's exact ordering and nil shape.
func (l *ClaimLedger) HistorySnapshotErr(context.Context) ([]ClaimEntry, error) {
	return l.HistorySnapshot(), nil
}

// ForRunAllErr preserves the file ledger's exact run filtering.
func (l *ClaimLedger) ForRunAllErr(_ context.Context, runID string) ([]ClaimEntry, error) {
	return l.ForRunAll(runID), nil
}

// deferClaimReadDecision leaves uncertainty pending, rather than turning it
// into either an empty observation or the generic timeout fallback run.
func (s *Scheduler) deferClaimReadDecision(poll demandPoll) {
	candidate := poll.candidate
	switch {
	case poll.schedule:
		candidate.scheduleRemaining = 0
		if candidate.queuedDemand != nil {
			// Durable demand keeps its prior count: do not ObserveDemand(0),
			// which would delete the obligation while its cursor stays advanced.
			candidate.demandWake = true
		} else {
			s.deferScheduleDemandPoll(candidate)
		}
	case poll.refill:
		candidate.refillEligible = 0
		candidate.refillObserved = false
	default:
		candidate.backlogRemaining = 0
		candidate.backlogObserved = false
	}
}
