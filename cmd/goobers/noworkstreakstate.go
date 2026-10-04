package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/nowork"
	"github.com/goobers/goobers/internal/stateclient"
	"github.com/goobers/goobers/providers"
)

// noworkstreakstate.go holds the per-item repeated-no-work counter (#5379).
//
// The defect it exists to close: an agentic stage that answers `no-work`
// completes its run at journal.PhaseCompleted, which releases the claim and
// leaves goobers:ready in place, so the next scheduler tick re-offers the same
// item and re-derives the same verdict. One unactionable item occupied a lane
// for two days across 28 claims and produced nothing.
//
// Why this is NOT folded into failurestreakstate.go's counter, even though the
// park it performs is the same label swap:
//
//  1. A no-work verdict is not a work failure. buildFailedHandler deliberately
//     skips ErrorClassItemJudgment for the failure streak; a verdict of
//     "nothing to do here" belongs to that same judgment family.
//  2. More decisively, the two counters have OPPOSING reset triggers. The
//     failure streak resets on journal.PhaseCompleted — and a no-work run IS a
//     PhaseCompleted run. Sharing one counter would mean each no-work
//     iteration reset the very streak it had just incremented, which is
//     precisely why the existing breaker never tripped on this loop no matter
//     how many times it went round.
//
// So the two live side by side: the failure streak keeps its completed-run
// reset exactly as it is (the #5379 scope decision requires that reset be
// preserved), and this counter resets only on a completion that actually
// produced work.
const (
	// noWorkStreakStateSchema versions the per-item document.
	noWorkStreakStateSchema = "goobers.dev/no-work-streak/v1"
	// noWorkStreakStateLockOperation labels the claims-lock critical section a
	// record's read-modify-write takes on the file backend.
	noWorkStreakStateLockOperation = "no-work-streak.update"
)

// noWorkStreakStateKey is the scheduler-state key holding one item's record,
// a pure function of noWorkStreakKey (provider/owner/name#itemID).
func noWorkStreakStateKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return stateclient.NoWorkStreakKey(fmt.Sprintf("%x", sum))
}

var noWorkStreakRecordSpec = keyedStateRecordSpec[nowork.Record]{
	schema:      noWorkStreakStateSchema,
	operation:   noWorkStreakStateLockOperation,
	errorPrefix: "decode no-work-streak state",
	field:       keyedStateRecordFieldRecord,
	stateKey:    noWorkStreakStateKey,
}

// noWorkStreakKey identifies one item's repeated-no-work state across
// providers and repos, so two identically-numbered issues in different repos
// never collide. Mirrors failureStreakKey's construction deliberately: the two
// records address the same item identity through different key families.
func noWorkStreakKey(repo providers.RepositoryRef, itemID string) string {
	return string(repo.Provider) + "/" + repo.Owner + "/" + repo.Name + "#" + itemID
}

// updateNoWorkStreakRecord is the record's read-modify-write: one lock
// acquisition on the file backend, one compare-and-swap on the plane. fn
// returns write=false to leave the key untouched, and MUST be safe to run more
// than once — the plane re-runs it against the new value when a CAS loses.
func updateNoWorkStreakRecord(
	ctx context.Context,
	store stateclient.Store,
	key string,
	fn func(nowork.Record) (nowork.Record, bool, error),
) error {
	return updateKeyedStateRecord(ctx, store, key, noWorkStreakRecordSpec,
		func(current nowork.Record, _ bool) (nowork.Record, bool, error) {
			return fn(current)
		})
}

// incrementNoWorkStreak advances an item's repeated-no-work count by one
// inside a single compare-and-swap and records terminal's verdict, returning
// the record it wrote and the record it replaced, so a concurrent terminal for
// the same item cannot lose an increment the way a read-then-write pair would.
//
// This is the load/modify/store split's counterpart to the failure streak's
// separate loadFailureStreakCount/writeFailureStreakCount: that pair is safe
// there because the daemon serializes failure terminals per item, but the
// no-work path is reached from completed terminals that carry no such
// guarantee, so the increment stays inside the CAS.
// Both records come from the SAME winning CAS invocation. Re-reading the
// record afterwards would open a read-after-write race: a concurrent terminal
// for the same item landing between the two calls would make the rendered
// count and the quoted reason come from different versions of the record — or,
// after a concurrent reset, quote nothing while claiming a full streak.
func incrementNoWorkStreak(
	ctx context.Context,
	l instance.Layout,
	repo providers.RepositoryRef,
	itemID string,
	runID string,
	terminal nowork.Terminal,
) (next, previous nowork.Record, err error) {
	store, err := openStageStateStore(l)
	if err != nil {
		return nowork.Record{}, nowork.Record{}, fmt.Errorf("open no-work-streak state: %w", err)
	}
	key := noWorkStreakKey(repo, itemID)
	if err := updateNoWorkStreakRecord(ctx, store, key,
		func(current nowork.Record) (nowork.Record, bool, error) {
			previous = current
			next = nowork.Advance(current, terminal, runID, time.Now().UTC())
			return next, true, nil
		}); err != nil {
		return nowork.Record{}, nowork.Record{}, err
	}
	return next, previous, nil
}

// loadNoWorkStreakRecord reads an item's recorded no-work verdict, the zero
// record when it has none. query-backlog uses it to hand the verdict to the
// next run on the item (#5643); the park path never re-reads (see
// incrementNoWorkStreak).
func loadNoWorkStreakRecord(
	ctx context.Context,
	l instance.Layout,
	repo providers.RepositoryRef,
	itemID string,
) (nowork.Record, error) {
	store, err := openStageStateStore(l)
	if err != nil {
		return nowork.Record{}, fmt.Errorf("open no-work-streak state: %w", err)
	}
	key := noWorkStreakKey(repo, itemID)
	value, err := store.Get(ctx, noWorkStreakStateKey(key))
	if err != nil {
		return nowork.Record{}, fmt.Errorf("read no-work-streak state for %s#%s: %w", repo.Name, itemID, err)
	}
	record, _, err := decodeKeyedStateRecord(value, key, noWorkStreakRecordSpec)
	return record, err
}

// resetNoWorkStreakState clears an item's repeated-no-work count after a
// productive completion and returns the record it cleared (the zero record
// when there was none), so the caller can flag the verdict this run
// contradicted (#5643). Idempotent by construction: an already-zero record
// (including an absent key) writes nothing, so the reset can be replayed by a
// retried terminal notification without churning the plane — and a replay
// returns the zero record, so the contradiction is flagged once.
func resetNoWorkStreakState(
	ctx context.Context,
	l instance.Layout,
	repo providers.RepositoryRef,
	itemID string,
	runID string,
) (nowork.Record, error) {
	store, err := openStageStateStore(l)
	if err != nil {
		return nowork.Record{}, fmt.Errorf("open no-work-streak state: %w", err)
	}
	key := noWorkStreakKey(repo, itemID)
	var cleared nowork.Record
	err = updateNoWorkStreakRecord(ctx, store, key,
		func(current nowork.Record) (nowork.Record, bool, error) {
			cleared = nowork.Record{}
			if current.Count == 0 {
				return nowork.Record{}, false, nil
			}
			cleared = current
			return nowork.Record{RunID: runID, UpdatedAt: time.Now().UTC()}, true, nil
		})
	return cleared, err
}
