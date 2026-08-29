package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/platform/durability"
	"github.com/goobers/goobers/providers"
)

// circuitBreakerOutboxFileName is the well-known file under an instance's
// scheduler/ directory recording circuit-breaker parks (#3646) whose provider
// mutation failed: the run reached its failure-streak threshold but applying
// goobers:needs-human / removing goobers:ready did not land. Without a durable
// record the protection is simply lost — the item stays eligible and the same
// unhealthy work churns every tick with no evidence anywhere that the breaker
// tried to fire. Sibling to claims.json and blocked.json, guarded by the same
// claims.lock, and drained by reconcileCircuitBreakerOutbox on the next
// terminal that runs the breaker.
const circuitBreakerOutboxFileName = "circuit-breaker-outbox.json"

// maxCircuitBreakerOutboxEntries bounds the outbox so a provider that is down
// for a long stretch cannot grow the file without limit. The oldest entries
// are dropped first: a newer unapplied park is the more urgent protection.
const maxCircuitBreakerOutboxEntries = 256

// maxCircuitBreakerOutboxErrorLen bounds the retained diagnostic. The error is
// for a human reading the file; the full text stays in the run journal.
const maxCircuitBreakerOutboxErrorLen = 512

// circuitBreakerMutation is one park that must still reach the provider.
type circuitBreakerMutation struct {
	Repository    providers.RepositoryRef `json:"repository"`
	ItemID        string                  `json:"itemId"`
	FailureStreak int                     `json:"failureStreak"`
	RunID         string                  `json:"runId"`
	Stage         string                  `json:"stage,omitempty"`
	RecordedAt    time.Time               `json:"recordedAt"`
	LastAttemptAt time.Time               `json:"lastAttemptAt"`
	Attempts      int                     `json:"attempts"`
	LastError     string                  `json:"lastError,omitempty"`
}

func circuitBreakerOutboxPath(l instance.Layout) string {
	return filepath.Join(l.SchedulerDir(), circuitBreakerOutboxFileName)
}

func loadCircuitBreakerOutbox(path string) (map[string]circuitBreakerMutation, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]circuitBreakerMutation{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	pending := map[string]circuitBreakerMutation{}
	if err := json.Unmarshal(data, &pending); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return pending, nil
}

func saveCircuitBreakerOutbox(path string, pending map[string]circuitBreakerMutation) error {
	data, err := json.MarshalIndent(pending, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal circuit breaker outbox: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	// Write-then-rename for the same torn-write reason blocked.json uses: a
	// crash mid-write must never leave a half-written file that fails every
	// subsequent reconciliation's parse.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := durability.ReplaceFile(tmp, path); err != nil {
		return fmt.Errorf("rename %s: %w", tmp, err)
	}
	return nil
}

// updateCircuitBreakerOutbox applies fn under the instance's claim lock and
// persists the result. fn returns false to skip the write.
func updateCircuitBreakerOutbox(l instance.Layout, fn func(pending map[string]circuitBreakerMutation) bool) error {
	path := circuitBreakerOutboxPath(l)
	return withClaimLock(filepath.Join(l.SchedulerDir(), claimLockFileName), claimLockOperationCircuitBreakerOutbox, func() error {
		pending, err := loadCircuitBreakerOutbox(path)
		if err != nil {
			return err
		}
		if !fn(pending) {
			return nil
		}
		return saveCircuitBreakerOutbox(path, pending)
	})
}

func snapshotCircuitBreakerOutbox(l instance.Layout) (map[string]circuitBreakerMutation, error) {
	var pending map[string]circuitBreakerMutation
	err := withClaimLock(filepath.Join(l.SchedulerDir(), claimLockFileName), claimLockOperationCircuitBreakerOutbox, func() error {
		var err error
		pending, err = loadCircuitBreakerOutbox(circuitBreakerOutboxPath(l))
		return err
	})
	if err != nil {
		return nil, err
	}
	return pending, nil
}

func boundCircuitBreakerOutboxError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if len(msg) > maxCircuitBreakerOutboxErrorLen {
		return msg[:maxCircuitBreakerOutboxErrorLen] + "…"
	}
	return msg
}

// recordCircuitBreakerMutationFailure persists a park that did not reach the
// provider so a later terminal can retry it. Re-recording the same item keeps
// its original RecordedAt and accumulates the attempt count.
func recordCircuitBreakerMutationFailure(l instance.Layout, repo providers.RepositoryRef, itemID, runID, stage string, streak int, cause error) error {
	key := blockedRecordKey(repo, itemID)
	now := time.Now().UTC()
	return updateCircuitBreakerOutbox(l, func(pending map[string]circuitBreakerMutation) bool {
		entry, ok := pending[key]
		if !ok {
			entry = circuitBreakerMutation{RecordedAt: now}
		}
		entry.Repository = repo
		entry.ItemID = itemID
		entry.RunID = runID
		entry.Stage = stage
		entry.FailureStreak = streak
		entry.LastAttemptAt = now
		entry.Attempts++
		entry.LastError = boundCircuitBreakerOutboxError(cause)
		pending[key] = entry
		evictOldestCircuitBreakerMutations(pending, key)
		return true
	})
}

// evictOldestCircuitBreakerMutations trims the outbox to its entry ceiling,
// dropping the oldest recordings first and never the entry just written.
func evictOldestCircuitBreakerMutations(pending map[string]circuitBreakerMutation, keep string) {
	if len(pending) <= maxCircuitBreakerOutboxEntries {
		return
	}
	keys := make([]string, 0, len(pending))
	for key := range pending {
		if key != keep {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := pending[keys[i]].RecordedAt, pending[keys[j]].RecordedAt
		if a.Equal(b) {
			return keys[i] < keys[j]
		}
		return a.Before(b)
	})
	for _, key := range keys {
		if len(pending) <= maxCircuitBreakerOutboxEntries {
			return
		}
		delete(pending, key)
	}
}

// clearCircuitBreakerMutations drops pending parks for the given items — the
// mutation landed, or a completed run reset the streak that motivated it.
func clearCircuitBreakerMutations(l instance.Layout, repo providers.RepositoryRef, itemIDs ...string) error {
	if len(itemIDs) == 0 {
		return nil
	}
	keys := make([]string, 0, len(itemIDs))
	for _, itemID := range itemIDs {
		keys = append(keys, blockedRecordKey(repo, itemID))
	}
	return updateCircuitBreakerOutbox(l, func(pending map[string]circuitBreakerMutation) bool {
		changed := false
		for _, key := range keys {
			if _, ok := pending[key]; ok {
				delete(pending, key)
				changed = true
			}
		}
		return changed
	})
}

// reconcileCircuitBreakerOutbox retries every park a previous terminal could
// not apply. It runs at the head of applyCircuitBreaker, so the retry cadence
// is "the next terminal that exercises the breaker" — the same cadence that
// would have re-parked the item anyway, except the protection is no longer
// lost when the item never fails again but is still unhealthy. A retry that
// fails again stays pending with an updated attempt count and diagnostic;
// success drops the entry.
func reconcileCircuitBreakerOutbox(ctx context.Context, poster gate.Commenter, l instance.Layout) error {
	pending, err := snapshotCircuitBreakerOutbox(l)
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		return nil
	}
	keys := make([]string, 0, len(pending))
	for key := range pending {
		keys = append(keys, key)
	}
	slices.Sort(keys)

	var errs []error
	for _, key := range keys {
		entry := pending[key]
		if _, err := poster.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
			Repository:   entry.Repository,
			ID:           entry.ItemID,
			AddLabels:    []string{providers.LabelNeedsHuman},
			RemoveLabels: []string{providers.LabelReady},
		}); err != nil {
			errs = append(errs, fmt.Errorf("retry circuit breaker on %s#%s: %w", entry.Repository.Name, entry.ItemID, err))
			if rerr := recordCircuitBreakerMutationFailure(l, entry.Repository, entry.ItemID, entry.RunID, entry.Stage, entry.FailureStreak, err); rerr != nil {
				errs = append(errs, rerr)
			}
			continue
		}
		if cerr := clearCircuitBreakerMutations(l, entry.Repository, entry.ItemID); cerr != nil {
			errs = append(errs, cerr)
		}
	}
	return errors.Join(errs...)
}
