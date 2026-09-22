package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	"github.com/goobers/goobers/internal/instance"
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

// noWorkStreakRecord is one item's authoritative repeated-no-work state.
//
// Reason carries the LAST recorded no-work rationale so the park comment can
// quote why the implementer kept declining, rather than parking an item with
// no explanation attached — the second half of #5379, which observed that the
// agentic stage journaled `status: no-work` with no outputs at all, leaving an
// operator unable to tell an unactionable item from a misread one.
type noWorkStreakRecord struct {
	Count     int       `json:"count"`
	Reason    string    `json:"reason,omitempty"`
	Stage     string    `json:"stage,omitempty"`
	RunID     string    `json:"runId,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// noWorkStreakDocument is one item's record as it lives at its own
// scheduler-state key, carrying the record key it was written for so a
// mis-keyed document is caught rather than acted on.
type noWorkStreakDocument struct {
	Schema string             `json:"schema"`
	Key    string             `json:"key"`
	Record noWorkStreakRecord `json:"record"`
}

// noWorkStreakStateKey is the scheduler-state key holding one item's record,
// a pure function of noWorkStreakKey (provider/owner/name#itemID).
func noWorkStreakStateKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return stateclient.NoWorkStreakKey(fmt.Sprintf("%x", sum))
}

// noWorkStreakKey identifies one item's repeated-no-work state across
// providers and repos, so two identically-numbered issues in different repos
// never collide. Mirrors failureStreakKey's construction deliberately: the two
// records address the same item identity through different key families.
func noWorkStreakKey(repo providers.RepositoryRef, itemID string) string {
	return string(repo.Provider) + "/" + repo.Owner + "/" + repo.Name + "#" + itemID
}

func decodeNoWorkStreakRecord(value stateclient.Value, key string) (noWorkStreakRecord, error) {
	if !value.Exists() {
		return noWorkStreakRecord{}, nil
	}
	var doc noWorkStreakDocument
	if err := json.Unmarshal(value.Data, &doc); err != nil {
		return noWorkStreakRecord{}, fmt.Errorf("decode no-work-streak state: %w", err)
	}
	if doc.Schema != noWorkStreakStateSchema {
		return noWorkStreakRecord{}, fmt.Errorf(
			"decode no-work-streak state: unsupported schema %q, want %q", doc.Schema, noWorkStreakStateSchema)
	}
	if doc.Key != key {
		return noWorkStreakRecord{}, fmt.Errorf(
			"decode no-work-streak state: record is keyed to %q, not %q", doc.Key, key)
	}
	return doc.Record, nil
}

func encodeNoWorkStreakRecord(key string, record noWorkStreakRecord) ([]byte, error) {
	return json.Marshal(noWorkStreakDocument{
		Schema: noWorkStreakStateSchema,
		Key:    key,
		Record: record,
	})
}

// updateNoWorkStreakRecord is the record's read-modify-write: one lock
// acquisition on the file backend, one compare-and-swap on the plane. fn
// returns write=false to leave the key untouched, and MUST be safe to run more
// than once — the plane re-runs it against the new value when a CAS loses.
func updateNoWorkStreakRecord(
	ctx context.Context,
	store stateclient.Store,
	key string,
	fn func(noWorkStreakRecord) (noWorkStreakRecord, bool, error),
) error {
	return store.Update(ctx, noWorkStreakStateKey(key), noWorkStreakStateLockOperation,
		func(value stateclient.Value) ([]byte, bool, error) {
			current, err := decodeNoWorkStreakRecord(value, key)
			if err != nil {
				return nil, false, err
			}
			next, write, err := fn(current)
			if err != nil || !write {
				return nil, false, err
			}
			data, err := encodeNoWorkStreakRecord(key, next)
			if err != nil {
				return nil, false, err
			}
			return data, true, nil
		})
}

// incrementNoWorkStreak advances an item's repeated-no-work count by one
// inside a single compare-and-swap and returns the resulting count, so a
// concurrent terminal for the same item cannot lose an increment the way a
// read-then-write pair would.
//
// This is the load/modify/store split's counterpart to the failure streak's
// separate loadFailureStreakCount/writeFailureStreakCount: that pair is safe
// there because the daemon serializes failure terminals per item, but the
// no-work path is reached from completed terminals that carry no such
// guarantee, so the increment stays inside the CAS.
// The resulting reason is returned alongside the count, from the SAME winning
// CAS invocation. Re-reading the record afterwards to recover it would open a
// read-after-write race: a concurrent terminal for the same item landing
// between the two calls would make the rendered count and the quoted reason
// come from different versions of the record — or, after a concurrent reset,
// quote nothing while claiming a full streak.
func incrementNoWorkStreak(
	ctx context.Context,
	l instance.Layout,
	repo providers.RepositoryRef,
	itemID string,
	runID, stage, reason string,
) (int, string, error) {
	store, err := openStageStateStore(l)
	if err != nil {
		return 0, "", fmt.Errorf("open no-work-streak state: %w", err)
	}
	key := noWorkStreakKey(repo, itemID)
	var count int
	var recorded string
	if err := updateNoWorkStreakRecord(ctx, store, key,
		func(current noWorkStreakRecord) (noWorkStreakRecord, bool, error) {
			count = current.Count + 1
			next := noWorkStreakRecord{
				Count:     count,
				Reason:    reason,
				Stage:     stage,
				RunID:     runID,
				UpdatedAt: time.Now().UTC(),
			}
			// An absent reason on this iteration must not erase a reason an
			// earlier iteration did record: the park comment is more useful
			// quoting a stale rationale than quoting nothing.
			if next.Reason == "" {
				next.Reason = current.Reason
			}
			recorded = next.Reason
			return next, true, nil
		}); err != nil {
		return 0, "", err
	}
	return count, recorded, nil
}

// resetNoWorkStreakState clears an item's repeated-no-work count after a
// productive completion. Idempotent by construction: an already-zero record
// (including an absent key) writes nothing, so the reset can be replayed by a
// retried terminal notification without churning the plane.
func resetNoWorkStreakState(
	ctx context.Context,
	l instance.Layout,
	repo providers.RepositoryRef,
	itemID string,
	runID string,
) error {
	store, err := openStageStateStore(l)
	if err != nil {
		return fmt.Errorf("open no-work-streak state: %w", err)
	}
	key := noWorkStreakKey(repo, itemID)
	return updateNoWorkStreakRecord(ctx, store, key,
		func(current noWorkStreakRecord) (noWorkStreakRecord, bool, error) {
			if current.Count == 0 {
				return noWorkStreakRecord{}, false, nil
			}
			return noWorkStreakRecord{RunID: runID, UpdatedAt: time.Now().UTC()}, true, nil
		})
}
