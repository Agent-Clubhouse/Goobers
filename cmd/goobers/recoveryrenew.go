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

// This runs even when FinalizeRun finds no worktree: earlier stage cleanup
// may have retained the only implementation before the run became terminal.
func renewTerminalRecovery(layout instance.Layout, runID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	entries, err := recovery.ReadInventory(ctx, filepath.Join(layout.Root, "recovery"), 128)
	if err != nil {
		return err
	}
	var matching []recovery.InventoryEntry
	for _, entry := range entries {
		if entry.Record.RunID == runID {
			matching = append(matching, entry)
		}
	}
	if len(matching) == 0 {
		return nil
	}
	reader, err := journal.OpenReadOnly(filepath.Join(layout.RunsDir(), runID))
	if err != nil {
		return err
	}
	identity, err := reader.Identity()
	if err != nil {
		return err
	}
	if identity.RunID != runID || identity.StartedAt.IsZero() {
		return fmt.Errorf("recovery renewal requires matching run identity")
	}
	events, err := reader.Events()
	if err != nil {
		return err
	}
	if journal.PhaseFromEvents(events) == journal.PhaseRunning {
		return nil
	}
	finishedAt, err := recoveryWindowTime(events, identity.StartedAt)
	if err != nil {
		return err
	}
	log := recoveryCleanupJournal{directory: layout.SchedulerDir(), scrubber: journal.NewRegistryScrubber()}
	for _, entry := range matching {
		if !entry.Record.RetainUntil.Before(finishedAt.Add(30 * 24 * time.Hour)) {
			continue
		}
		record, err := recovery.RenewRetention(ctx, entry.RecordPath, finishedAt.Add(30*24*time.Hour), 512<<20)
		if err != nil {
			return err
		}
		event, err := recovery.RetainedEvent(record)
		if err != nil {
			return err
		}
		if err := log.Append(event); err != nil {
			return err
		}
	}
	return nil
}
