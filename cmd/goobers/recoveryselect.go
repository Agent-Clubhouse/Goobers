package main

import (
	"context"
	"fmt"
	"path/filepath"
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
	var selected recovery.InventoryEntry
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
		if selected.RecordPath != "" && selected.Record.CreatedAt.Equal(entry.Record.CreatedAt) {
			return recovery.InventoryEntry{}, fmt.Errorf("multiple recovery snapshots have the same capture time; select an explicit record")
		}
		if selected.RecordPath == "" || entry.Record.CreatedAt.After(selected.Record.CreatedAt) {
			selected = entry
		}
	}
	if selected.RecordPath == "" {
		return recovery.InventoryEntry{}, fmt.Errorf("no unexpired terminal recovery snapshot matches the claimed issue")
	}
	return selected, nil
}
