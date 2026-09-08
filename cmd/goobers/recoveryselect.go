package main

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
)

// selectIssueRecovery resolves a retained candidate for an already-authorized
// issue claim. It does not claim an issue, prove absence of a provider PR, or
// authorize access from another run's pod. Callers must provide those checks.
// Archive integrity and availability are verified again by restoration.
func selectIssueRecovery(ctx context.Context, layout instance.Layout, repositoryKey, issueID string, now time.Time) (recovery.InventoryEntry, error) {
	entries, err := recovery.ReadInventory(ctx, filepath.Join(layout.Root, "recovery"), 128)
	if err != nil {
		return recovery.InventoryEntry{}, err
	}
	// One history read for the bounded candidate set, never one per snapshot.
	events, err := journal.ReadInstanceLog(layout.SchedulerDir())
	if err != nil {
		return recovery.InventoryEntry{}, err
	}
	var candidates []recovery.InventoryEntry
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return recovery.InventoryEntry{}, err
		}
		if entry.Record.RepositoryKey != repositoryKey {
			continue
		}
		matched, err := recoveryClaimMatches(events, entry.Record.RunID, repositoryKey, issueID)
		if err != nil {
			return recovery.InventoryEntry{}, err
		}
		if !matched {
			continue
		}
		entry.Record, err = recovery.ReadRetainedRecord(entry.RecordPath)
		if err != nil {
			return recovery.InventoryEntry{}, err
		}
		if !now.Before(entry.Record.RetainUntil) {
			continue
		}
		dir, err := runDirFor(layout, entry.Record.RunID)
		if err != nil {
			return recovery.InventoryEntry{}, err
		}
		reader, err := journal.OpenReadOnly(dir)
		if err != nil {
			return recovery.InventoryEntry{}, err
		}
		identity, err := reader.Identity()
		if err != nil || identity.RunID != entry.Record.RunID {
			return recovery.InventoryEntry{}, fmt.Errorf("recovery candidate journal identity mismatch")
		}
		phase, err := reader.PhaseBounded(ctx)
		if err != nil {
			return recovery.InventoryEntry{}, err
		}
		if !terminalRunPhase(phase) {
			continue
		}
		candidates = append(candidates, entry)
	}
	if len(candidates) == 0 {
		return recovery.InventoryEntry{}, fmt.Errorf("no unexpired terminal recovery snapshot matches the claimed issue")
	}
	// Resolve ties only at the newest capture time. Older ambiguous captures
	// must not prevent selecting a later, independently identified run.
	slices.SortFunc(candidates, func(a, b recovery.InventoryEntry) int { return b.Record.CreatedAt.Compare(a.Record.CreatedAt) })
	selected := candidates[0]
	for _, entry := range candidates[1:] {
		if !entry.Record.CreatedAt.Equal(selected.Record.CreatedAt) {
			break
		}
		if entry.Record.RunID != selected.Record.RunID {
			return recovery.InventoryEntry{}, fmt.Errorf("multiple recovery runs have the same capture time; select an explicit record")
		}
		prior, err := recoveryCaptureOrder(ctx, events, selected.Record)
		if err != nil {
			return recovery.InventoryEntry{}, err
		}
		next, err := recoveryCaptureOrder(ctx, events, entry.Record)
		if err != nil {
			return recovery.InventoryEntry{}, err
		}
		if next > prior {
			selected = entry
		}
	}
	return selected, nil
}

// Stage snapshots share the run's immutable start timestamp. Explicit capture
// observations order source state, including a deliberate return to an earlier
// snapshot. Renewal/readback events must never reorder captured implementation.
func recoveryCaptureOrder(ctx context.Context, events []journal.Event, expected recovery.Record) (int, error) {
	latest := -1
	for index, event := range events {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if event.Runner["recoveryCapture"] != true || event.RunID != expected.RunID || event.Runner["recoveryRepositoryKey"] != expected.RepositoryKey || event.Runner["recoveryRef"] != expected.Ref {
			continue
		}
		records, err := recovery.RecordsFromEvents([]journal.Event{event}, expected.RunID)
		if err != nil {
			return 0, err
		}
		if len(records) == 0 {
			continue
		}
		observed := records[0]
		observed.RetainUntil = expected.RetainUntil
		observed.CreatedAt = observed.CreatedAt.UTC()
		expected.CreatedAt = expected.CreatedAt.UTC()
		if observed != expected {
			return 0, recovery.ErrRecordConflict
		}
		latest = index
	}
	if latest >= 0 {
		return latest, nil
	}
	return 0, fmt.Errorf("recovery capture order lacks a matching durable observation; select an explicit record")
}
