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

// renewableRecoveryEntries finds the reservations this run owns.
//
// It reads at the structural ceiling, tolerantly, because renewal touches only
// its OWN records. Reading at the operator cap refused outright once the
// directory held more entries than the cap ("10 of 8 slots used"), and because
// renewTerminalRecovery's failure is joined as ErrCleanupDeferred, terminal
// finalization of every completed run was deferred and retried at every
// startup — each deferral holding the worktree and active marker it was trying
// to release (#5354). A run whose record is absent has nothing to renew, which
// is a no-op, never a refusal; an entry no scan can interpret carries no run
// identity, so it is not this run's and skipping it renews nothing.
//
// Overflow records are deliberately not included: renewal rebinds a record to
// its archive (recovery.RenewRetention verifies the bundle's size and digest),
// and an overflow entry holds a pinned ref instead of an archive, so there is
// nothing for renewal to rebind. That an overflow record's deadline therefore
// does not extend at terminal finalization is #5370 behaviour, unchanged here.
func renewableRecoveryEntries(ctx context.Context, root, runID string) ([]recovery.InventoryEntry, error) {
	entries, _, err := recovery.ReadInventoryTolerant(ctx, root, recovery.MaxInventoryEntries)
	if err != nil {
		return nil, err
	}
	var matching []recovery.InventoryEntry
	for _, entry := range entries {
		if entry.Record.RunID == runID {
			matching = append(matching, entry)
		}
	}
	return matching, nil
}

// This runs even when FinalizeRun finds no worktree: earlier stage cleanup
// may have retained the only implementation before the run became terminal.
func renewTerminalRecovery(layout instance.Layout, runID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	// This safety net must not start failing terminal renewal just because
	// config happens to be unavailable at this exact moment — it never
	// needed config before #4823 introduced tunable limits. Falling back to
	// the same defaults config would resolve to (RecoverySnapshotConfig's
	// zero value) keeps that pre-#4823 resilience — but the fallback is
	// journaled rather than silent (#5092): reading this instance-wide
	// inventory at the built-in 128 while it legitimately holds thousands of
	// entries fails as "inventory is full", which pins the run's active
	// marker and so inflates the next restart's crash-resume candidate set
	// (#5199).
	log := recoveryCleanupJournal{directory: layout.SchedulerDir(), scrubber: journal.NewRegistryScrubber()}
	root := filepath.Join(layout.Root, "recovery")
	recoveryCfg, origin := resolveRecoveryPolicy(layout, nil)
	journalRecoveryPolicyFallback(log, origin, recoveryCfg, root)
	retainWindow, err := recoveryCfg.RetainWindowEffective()
	if err != nil {
		return err
	}
	matching, err := renewableRecoveryEntries(ctx, root, runID)
	if err != nil {
		return err
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
