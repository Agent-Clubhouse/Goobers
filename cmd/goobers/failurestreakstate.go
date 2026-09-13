package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/stateclient"
	"github.com/goobers/goobers/providers"
)

// failurestreakstate.go moves the failure-streak counter that trips the
// circuit breaker (goobers:needs-human, ready removed) off the pre-#3025
// mechanisms — a provider-comment marker anyone with comment access could
// edit or delete, and an instance-journal fold that required an instance
// root a stage pod does not have — onto the closed scheduler-state KV plane
// every other cross-run counter in this family already uses.
//
// The provider comment (internal/gate.UpsertFailureComment/ResetFailureComment)
// stays: it is the human-visible half, now written strictly AFTER the
// authoritative key update succeeds, so it is a best-effort PROJECTION that
// can be retried, reprojected, or simply disagree with the record without
// changing scheduling behavior — never the value a scheduling decision reads
// back.
const (
	// failureStreakStateSchema versions the per-item document.
	failureStreakStateSchema = "goobers.dev/failure-streak/v1"
	// failureStreakStateLockOperation labels the claims-lock critical section
	// a streak record's read-modify-write takes on the file backend, alongside
	// remediationnoopguard.go's own operation label.
	failureStreakStateLockOperation = "failure-streak.update"
)

// failureStreakRecord is one item's authoritative streak state.
type failureStreakRecord struct {
	Count     int       `json:"count"`
	Stage     string    `json:"stage,omitempty"`
	RunID     string    `json:"runId,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func (r failureStreakRecord) empty() bool { return r == failureStreakRecord{} }

// failureStreakDocument is one item's record as it lives at its own
// scheduler-state key, carrying the record key it was written for so a
// mis-keyed document is caught rather than acted on (the same integrity
// posture remediationNoopDocument takes).
type failureStreakDocument struct {
	Schema string              `json:"schema"`
	Key    string              `json:"key"`
	Record failureStreakRecord `json:"record"`
}

// failureStreakStateKey is the scheduler-state key holding one item's record.
// A pure function of failureStreakKey (provider/owner/name#itemID), which is
// already the record's canonical identity, so the same item reaches the same
// key whether the caller is the daemon's own circuit breaker or (in the
// future) a stage pod reading the record through the plane.
func failureStreakStateKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return stateclient.FailureStreakKey(fmt.Sprintf("%x", sum))
}

func decodeFailureStreakRecord(value stateclient.Value, key string) (failureStreakRecord, error) {
	if !value.Exists() {
		return failureStreakRecord{}, nil
	}
	var doc failureStreakDocument
	if err := json.Unmarshal(value.Data, &doc); err != nil {
		return failureStreakRecord{}, fmt.Errorf("decode failure-streak state: %w", err)
	}
	if doc.Schema != failureStreakStateSchema {
		return failureStreakRecord{}, fmt.Errorf(
			"decode failure-streak state: unsupported schema %q, want %q", doc.Schema, failureStreakStateSchema)
	}
	if doc.Key != key {
		return failureStreakRecord{}, fmt.Errorf(
			"decode failure-streak state: record is keyed to %q, not %q", doc.Key, key)
	}
	return doc.Record, nil
}

func encodeFailureStreakRecord(key string, record failureStreakRecord) ([]byte, error) {
	return json.Marshal(failureStreakDocument{
		Schema: failureStreakStateSchema,
		Key:    key,
		Record: record,
	})
}

// updateFailureStreakRecord is the record's read-modify-write: one lock
// acquisition on the file backend, one compare-and-swap on the plane. fn
// returns write=false to leave the key untouched, and MUST be safe to run more
// than once — the plane re-runs it against the new value when a CAS loses.
func updateFailureStreakRecord(
	ctx context.Context,
	store stateclient.Store,
	key string,
	fn func(failureStreakRecord) (failureStreakRecord, bool, error),
) error {
	return store.Update(ctx, failureStreakStateKey(key), failureStreakStateLockOperation,
		func(value stateclient.Value) ([]byte, bool, error) {
			current, err := decodeFailureStreakRecord(value, key)
			if err != nil {
				return nil, false, err
			}
			next, write, err := fn(current)
			if err != nil || !write {
				return nil, false, err
			}
			data, err := encodeFailureStreakRecord(key, next)
			if err != nil {
				return nil, false, err
			}
			return data, true, nil
		})
}

// loadFailureStreakState reads an item's authoritative streak record. An
// absent key falls back to the legacy failure-streak comment marker
// (Goobers#3025's one-release migration-on-read): the value is parsed,
// validated, written into the new key with a compare-and-swap, and a journal
// event records the migration. A marker that cannot be parsed (missing,
// malformed) is treated as "nothing to migrate", never as a count of zero
// overriding a value that could not be read — see gate.ParseFailureStreakCount.
//
// Once the key exists, the provider comment is never consulted again: editing
// or deleting it cannot change what this returns.
func loadFailureStreakState(
	ctx context.Context,
	store stateclient.Store,
	poster gate.Commenter,
	l instance.Layout,
	repo providers.RepositoryRef,
	itemID string,
) (failureStreakRecord, error) {
	key := failureStreakKey(repo, itemID)
	value, err := store.Get(ctx, failureStreakStateKey(key))
	if err != nil {
		return failureStreakRecord{}, fmt.Errorf("read failure-streak state for %s#%s: %w", repo.Name, itemID, err)
	}
	if value.Exists() {
		return decodeFailureStreakRecord(value, key)
	}
	if poster == nil {
		return failureStreakRecord{}, nil
	}
	// The migration consult is best-effort, deliberately NOT propagated as a
	// load error: #4364's whole point was that a rate-limited provider call
	// must never read back as a wrong count. Reusing the record's absence to
	// mean "count is zero" here would resurrect exactly that bug one layer up
	// — a transient ListComments failure would look identical to "this item
	// has never failed" instead of "the one-time migration hasn't happened
	// yet". Returning the zero record without writing anything lets the next
	// read retry the migration instead.
	comments, err := poster.ListComments(ctx, repo, itemID)
	if err != nil {
		return failureStreakRecord{}, nil
	}
	var legacyCount int
	var found bool
	for _, c := range comments {
		if count, ok := gate.ParseFailureStreakCount(c.Body); ok {
			legacyCount = count
			found = true
			break
		}
	}
	if !found || legacyCount == 0 {
		return failureStreakRecord{}, nil
	}
	migrated := failureStreakRecord{Count: legacyCount, UpdatedAt: time.Now().UTC()}
	if err := updateFailureStreakRecord(ctx, store, key, func(current failureStreakRecord) (failureStreakRecord, bool, error) {
		if !current.empty() {
			// A concurrent writer already migrated or advanced this record;
			// its value wins.
			migrated = current
			return current, false, nil
		}
		return migrated, true, nil
	}); err != nil {
		return failureStreakRecord{}, fmt.Errorf("migrate legacy failure-streak state for %s#%s: %w", repo.Name, itemID, err)
	}
	if annotations, aerr := openStageAnnotator(l); aerr == nil {
		_ = annotations.Append(journal.Event{
			Type: journal.EventRunnerAnnotation,
			Runner: map[string]any{
				"annotation": "failure-streak-migrated",
				"key":        key,
				"count":      migrated.Count,
			},
		})
		_ = annotations.Close()
	}
	return migrated, nil
}

// writeFailureStreakState is the record's authoritative write, called AFTER
// which the caller posts the human-visible comment projection. count is the
// value the record is set to (not incremented — the caller already resolved
// the new count from loadFailureStreakState's return).
func writeFailureStreakState(
	ctx context.Context,
	store stateclient.Store,
	l instance.Layout,
	repo providers.RepositoryRef,
	itemID string,
	count int,
	runID, stage string,
) error {
	key := failureStreakKey(repo, itemID)
	if err := updateFailureStreakRecord(ctx, store, key, func(failureStreakRecord) (failureStreakRecord, bool, error) {
		return failureStreakRecord{Count: count, Stage: stage, RunID: runID, UpdatedAt: time.Now().UTC()}, true, nil
	}); err != nil {
		return err
	}
	// Best-effort audit trail (successful authoritative transitions get a
	// journal event, per Goobers#3025's acceptance criteria) using the same
	// stage-pod-safe annotation seam #3898 already built — it works from a
	// pod (over the run-scoped journal plane) exactly as it does from the
	// daemon's own process.
	annotations, err := openStageAnnotator(l)
	if err != nil {
		return nil
	}
	defer func() { _ = annotations.Close() }()
	_ = annotations.Append(journal.Event{
		Type:  journal.EventRunnerAnnotation,
		RunID: runID,
		Stage: stage,
		Runner: map[string]any{
			"annotation": failureStreakAnnotation,
			"key":        key,
			"count":      count,
		},
	})
	return nil
}
