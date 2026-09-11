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
	// This safety net must not start failing terminal renewal just because
	// config happens to be unavailable at this exact moment — it never
	// needed config before #4823 introduced tunable limits. Falling back to
	// the same defaults config would resolve to (RecoverySnapshotConfig's
	// zero value) keeps that pre-#4823 resilience.
	recoveryCfg := instance.RecoverySnapshotConfig{}
	if cfg, err := instance.LoadConfig(layout.ConfigFile()); err == nil {
		recoveryCfg = cfg.Retention.RecoveryEffective()
	}
	retainWindow, err := recoveryCfg.RetainWindowEffective()
	if err != nil {
		return err
	}
	entries, err := recovery.ReadInventory(ctx, filepath.Join(layout.Root, "recovery"), recoveryCfg.MaxSnapshotsEffective())
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
		if !entry.Record.RetainUntil.Before(finishedAt.Add(retainWindow)) {
			continue
		}
		record, err := recovery.RenewRetention(ctx, entry.RecordPath, finishedAt.Add(retainWindow), recoveryCfg.MaxArchiveBytesEffective())
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
