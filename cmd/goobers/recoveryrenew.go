package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/worktree"
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
// Overflow records are read separately (renewableOverflowEntries): renewal
// here rebinds a record to its archive (recovery.RenewRetention verifies the
// bundle's size and digest), and an overflow entry holds a pinned ref instead
// of an archive, so it is renewed by recovery.RenewOverflowRetention instead.
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

// renewableOverflowEntries finds this run's overflow-tier records. Like the
// inventory read above it is tolerant: an entry no scan can interpret carries
// no run identity, so it is not this run's.
func renewableOverflowEntries(ctx context.Context, layout instance.Layout, runID string) ([]recovery.InventoryEntry, error) {
	entries, _, err := readConfiguredRecoveryOverflow(ctx, layout)
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
//
// Both durability tiers are renewed to the same deadline (#5403). The
// overflow tier is read and renewed BEFORE the inventory is read for
// renewal, so a promotion racing this renewal cannot strand the extension: a
// promotion that completed before the overflow read, or published its bundle
// before the overflow record moved, is found by the later inventory read; one
// that publishes after the record moved carries it (promotion re-reads the
// overflow record after publishing; see promoteRecoveryOverflowEntry).
func renewTerminalRecovery(layout instance.Layout, manager *worktree.Manager, runID string) error {
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
	overflow := terminalOverflowEntries(ctx, layout, log, runID)
	matching, err := renewableRecoveryEntries(ctx, root, runID)
	if err != nil {
		return err
	}
	if len(matching) == 0 && len(overflow) == 0 {
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
	deadline := finishedAt.Add(retainWindow)
	if len(overflow) > 0 {
		if err := renewTerminalOverflow(ctx, layout, manager, log, overflow, deadline); err != nil {
			return err
		}
		if matching, err = renewableRecoveryEntries(ctx, root, runID); err != nil {
			return err
		}
	}
	for _, entry := range matching {
		if !entry.Record.RetainUntil.Before(deadline) {
			continue
		}
		record, err := recovery.RenewRetention(ctx, entry.RecordPath, deadline, recoveryCfg.MaxArchiveBytesEffective())
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

// terminalOverflowEntries reads this run's overflow records for renewal.
// Terminal renewal never read the overflow tier before #5403, and that tier
// is instance-wide, so a root it cannot read must not start deferring every
// run's terminal finalization: the failure is journaled and the run's
// overflow records, if any, keep their capture-time deadline.
func terminalOverflowEntries(ctx context.Context, layout instance.Layout, log recoveryCleanupJournal, runID string) []recovery.InventoryEntry {
	overflow, err := renewableOverflowEntries(ctx, layout, runID)
	if err != nil {
		journalOverflowRenewalSkipped(log, runID, "", fmt.Sprintf("overflow tier unreadable: %v", err))
		return nil
	}
	return overflow
}

// renewTerminalOverflow extends this run's overflow records to deadline after
// verifying, in the managed repository that holds it, that each record's pin
// still resolves to its snapshot (#5403).
//
// Locating that repository is best-effort, deliberately. A bundle-backed
// renewal needs nothing but the inventory, while this one also needs the
// instance config and a managed copy; a renewal failure is joined as
// ErrCleanupDeferred, and deferring terminal finalization holds the worktree
// and active marker it is trying to release (#5354). So an entry whose
// repository cannot be resolved or located, or whose pin no longer resolves,
// keeps its capture-time deadline — exactly its pre-#5403 behaviour — and the
// skip is journaled so the unrenewed deadline is explicable. Only a failure to
// write or journal a verified extension is returned.
func renewTerminalOverflow(ctx context.Context, layout instance.Layout, manager *worktree.Manager, log recoveryCleanupJournal, entries []recovery.InventoryEntry, deadline time.Time) error {
	cfg, cfgErr := instance.LoadConfig(layout.ConfigFile())
	for _, entry := range entries {
		if !entry.Record.RetainUntil.Before(deadline) {
			continue
		}
		var renewed *recovery.Record
		skipped := "no managed repository manager"
		switch {
		case cfgErr != nil:
			skipped = fmt.Sprintf("instance configuration unavailable: %v", cfgErr)
		case manager != nil:
			var err error
			renewed, skipped, err = renewOverflowEntry(ctx, cfg, manager, entry, deadline)
			if err != nil {
				return err
			}
		}
		if renewed == nil {
			if skipped != "" {
				journalOverflowRenewalSkipped(log, entry.Record.RunID, entry.Record.Ref, skipped)
			}
			continue
		}
		event, err := recovery.RetainedEvent(*renewed)
		if err != nil {
			return err
		}
		// The same tier marker the capture acknowledgement carries, so the
		// renewal is never mistaken for bundle-durable work.
		event.Runner["recoveryOverflow"] = true
		if err := log.Append(event); err != nil {
			return err
		}
	}
	return nil
}

// renewOverflowEntry renews one overflow entry in whichever managed copy
// still pins it, under the same per-repository lock promotion holds. A nil
// record with a nil error means nothing was renewed; skipped then says why,
// and is empty when the record was promoted or retired meanwhile, which is
// not a skip (the later inventory read renews a promoted bundle).
func renewOverflowEntry(ctx context.Context, cfg *instance.Config, manager *worktree.Manager, entry recovery.InventoryEntry, deadline time.Time) (renewed *recovery.Record, skipped string, err error) {
	url, err := recoveryRetentionCloneURL(cfg, entry.Record.RepositoryKey)
	if err != nil {
		return nil, fmt.Sprintf("recovery repository unresolved: %v", err), nil
	}
	skipped = "no managed repository holds the snapshot ref"
	var renewErr error
	found, err := manager.WithRecoveryRepositories(ctx, url, func(repositories []string) error {
		source, _ := recoveryOverflowSource(ctx, repositories, entry.Record)
		if source == "" {
			return nil
		}
		record, err := recovery.RenewOverflowRetention(ctx, source, entry.RecordPath, deadline)
		switch {
		case errors.Is(err, os.ErrNotExist):
			skipped = ""
		case errors.Is(err, recovery.ErrOverflowRefUnresolved):
			// Already the default skip reason.
		case err != nil:
			renewErr = err
		default:
			renewed, skipped = &record, ""
		}
		return nil
	})
	if renewErr != nil {
		return nil, "", renewErr
	}
	if err != nil {
		return nil, fmt.Sprintf("managed repositories unavailable: %v", err), nil
	}
	if !found {
		return nil, "no managed repository exists", nil
	}
	return renewed, skipped, nil
}

// journalOverflowRenewalSkipped records why an overflow record kept its
// capture-time deadline at terminal finalization. Best-effort by
// construction, like journalRecoveryPolicyFallback: the explanation must not
// become the reason finalization is deferred.
func journalOverflowRenewalSkipped(log recoveryCleanupJournal, runID, ref, reason string) {
	subject := "run " + runID
	if ref != "" {
		subject += " ref " + ref
	}
	message := fmt.Sprintf("recovery overflow record for %s not renewed at terminal finalization: %s", subject, reason)
	_ = log.Append(journal.Event{
		Type:  journal.EventError,
		Error: &journal.ErrorDetail{Code: "recovery_overflow_renewal_skipped", Message: message},
	})
}
