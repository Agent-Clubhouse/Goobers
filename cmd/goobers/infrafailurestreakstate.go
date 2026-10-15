package main

import (
	"context"
	"fmt"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

// infraFailureStreakAnnotation marks the journal audit trail of the separate
// infrastructure failure streak (#5638), alongside failureStreakAnnotation.
const infraFailureStreakAnnotation = "infra-failure-streak"

// infraFailureStreakKey is the infrastructure streak's identity: the work
// streak's canonical key with a suffix no item id can produce, so the two
// counters live side by side in the same scheduler-state plane and record
// shape without ever sharing a key.
func infraFailureStreakKey(repo providers.RepositoryRef, itemID string) string {
	return failureStreakKey(repo, itemID) + "/infrastructure"
}

func (s escalationNotifyState) LoadInfraFailureStreak(ctx context.Context, repo providers.RepositoryRef, itemID string) (int, error) {
	return loadInfraFailureStreakCount(ctx, s.layout, repo, itemID)
}

func (s escalationNotifyState) WriteInfraFailureStreak(repo providers.RepositoryRef, itemID string, count int, runID, stage string) error {
	return writeInfraFailureStreakCount(s.layout, repo, itemID, count, runID, stage)
}

// loadInfraFailureStreakCount returns an item's infrastructure streak, or 0
// when none was ever recorded. Unlike the work streak there is no legacy
// comment to migrate: this counter is new with #5638.
func loadInfraFailureStreakCount(ctx context.Context, l instance.Layout, repo providers.RepositoryRef, itemID string) (int, error) {
	store, err := openStageStateStore(l)
	if err != nil {
		return 0, fmt.Errorf("open infrastructure failure-streak state: %w", err)
	}
	key := infraFailureStreakKey(repo, itemID)
	value, err := store.Get(ctx, failureStreakStateKey(key))
	if err != nil {
		return 0, fmt.Errorf("read infrastructure failure-streak state for %s#%s: %w", repo.Name, itemID, err)
	}
	if !value.Exists() {
		return 0, nil
	}
	record, err := decodeFailureStreakRecord(value, key)
	return record.Count, err
}

// writeInfraFailureStreakCount sets an item's infrastructure streak to count,
// the authoritative write the human-visible comment follows.
func writeInfraFailureStreakCount(l instance.Layout, repo providers.RepositoryRef, itemID string, count int, runID, stage string) error {
	store, err := openStageStateStore(l)
	if err != nil {
		return fmt.Errorf("open infrastructure failure-streak state: %w", err)
	}
	key := infraFailureStreakKey(repo, itemID)
	if err := updateFailureStreakRecord(stateContext(), store, key, func(failureStreakRecord) (failureStreakRecord, bool, error) {
		return failureStreakRecord{Count: count, Stage: stage, RunID: runID, UpdatedAt: time.Now().UTC()}, true, nil
	}); err != nil {
		return err
	}
	annotations, err := openStageAnnotator(l)
	if err != nil {
		return nil
	}
	defer func() { _ = annotations.Close() }()
	_ = annotations.Append(journal.Event{
		Type:   journal.EventRunnerAnnotation,
		RunID:  runID,
		Stage:  stage,
		Runner: map[string]any{"annotation": infraFailureStreakAnnotation, "key": key, "count": count},
	})
	return nil
}
