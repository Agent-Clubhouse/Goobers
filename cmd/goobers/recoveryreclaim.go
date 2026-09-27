package main

import (
	"context"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
)

// The justifications a reclamation may record. A slot is never freed without
// one: #5354's non-goals rule out "any reclamation that cannot state why the
// content it removes was not worth keeping".
const (
	// reclaimOperatorAbandoned is an operator's explicit decision, taken with
	// `goobers recovery-abandon` against one exact record.
	reclaimOperatorAbandoned = "operator-abandoned"
	// reclaimStoredNoDiff is a capture whose tree matched its base exactly,
	// so it holds no patch bytes at all (#5095).
	reclaimStoredNoDiff = "stored-no-diff"
	// reclaimBookkeepingOnly is a capture whose patch touches only Goobers'
	// own stage outputs, never repository content (#5119).
	reclaimBookkeepingOnly = "bookkeeping-only"
	// reclaimSupersededDuplicate is a capture a NEWER retained entry protects
	// byte for byte, so retiring it discards nothing (#5110).
	reclaimSupersededDuplicate = "superseded-duplicate"
	// reclaimLandingProven is the original #4823 justification: the work is
	// already present on the target branch.
	reclaimLandingProven = "landing-proven"
)

// recoveryContentlessJustification names why record holds nothing worth
// keeping, judged ONLY from retained content — no landing proof, no operator
// decision, no elapsed retention window. It returns "" when the entry must be
// kept, which includes every case where the evidence is unavailable: an
// unreadable snapshot is never read as an empty one.
//
// It is deliberately independent of the caller's timing so the on-demand
// eviction hook and the periodic retention sweep cannot disagree about what
// counts as contentless. The sweep does not call it yet — recoveryexpiry.go's
// recoveryRetirementEligible is owned by a sibling change (#5359) — and this
// is the seam it should adopt.
//
// retained is every currently retained record, needed to decide supersession;
// operatorEvents establishes that a superseding entry is not itself already
// abandoned. repositories is the managed mirror and pinned clone set for
// record's OWN repository, held by the caller; ownRepository reports whether
// the caller holds that repository's lock for its own cleanup, which is the
// only case where listing the snapshot's paths is in scope.
func recoveryContentlessJustification(ctx context.Context, record recovery.Record, repositories []string, retained []recovery.Record, operatorEvents []journal.Event, ownRepository bool) string {
	if record.HasNoDiff() {
		return reclaimStoredNoDiff
	}
	if ownRepository && bookkeepingOnlyRecord(ctx, record, repositories) {
		return reclaimBookkeepingOnly
	}
	if supersededDuplicateRecord(record, retained, operatorEvents) {
		return reclaimSupersededDuplicate
	}
	return ""
}

// bookkeepingOnlyRecord fails closed: if no managed copy can list the
// snapshot's paths, the entry is not reclaimable. An unavailable object is
// never evidence that there was nothing to keep.
func bookkeepingOnlyRecord(ctx context.Context, record recovery.Record, repositories []string) bool {
	for _, repository := range repositories {
		paths, err := recovery.SnapshotTouchedPaths(ctx, repository, record)
		if err != nil {
			continue
		}
		return recovery.BookkeepingOnlySnapshot(paths)
	}
	return false
}

// supersededDuplicateRecord reports whether a NEWER retained, non-abandoned
// entry carries identical content for the same repository and base. Retiring
// the older one discards nothing: the identical bytes stay protected by the
// newer entry. The newest member of a duplicate set is never a candidate, so
// a set can only ever shrink to one — never to zero.
func supersededDuplicateRecord(record recovery.Record, retained []recovery.Record, operatorEvents []journal.Event) bool {
	for _, other := range retained {
		if !sameRetainedContent(record, other) || !newerRetainedEntry(other, record) {
			continue
		}
		// The survivor must be one no operator has already abandoned, or a
		// whole duplicate set could collapse behind an entry that is itself
		// about to be removed.
		abandoned, err := recovery.ExplicitlyAbandoned(operatorEvents, other)
		if err == nil && !abandoned {
			return true
		}
	}
	return false
}

func sameRetainedContent(a, b recovery.Record) bool {
	return a.RepositoryKey == b.RepositoryKey && a.BaseSHA == b.BaseSHA && a.PatchDigest == b.PatchDigest
}

// newerRetainedEntry breaks a capture-time tie on snapshot identity, so two
// entries recorded in the same instant can never each be "superseded" by the
// other and both be retired.
func newerRetainedEntry(candidate, than recovery.Record) bool {
	if !candidate.CreatedAt.Equal(than.CreatedAt) {
		return candidate.CreatedAt.After(than.CreatedAt)
	}
	return candidate.SnapshotSHA > than.SnapshotSHA
}
