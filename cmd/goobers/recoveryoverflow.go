package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/worktree"
)

// readConfiguredRecoveryOverflow reads the overflow tier for this instance.
// It is tolerant by construction (recovery.ReadOverflow), for the same reason
// eviction's inventory read is: one unreadable entry must not hide the rest,
// and an entry nothing can interpret is never a promotion candidate anyway.
func readConfiguredRecoveryOverflow(ctx context.Context, layout instance.Layout) ([]recovery.InventoryEntry, []recovery.UnreadableEntry, error) {
	return recovery.ReadOverflow(ctx, recoveryOverflowRoot(layout))
}

// recoveryOverflowCount reports how many entries sit at the ref tier, and
// whether that reading could be taken at all. Callers report occupancy, so an
// unreadable overflow root must not be indistinguishable from an empty one.
func recoveryOverflowCount(ctx context.Context, layout instance.Layout) (int, error) {
	entries, unreadable, err := readConfiguredRecoveryOverflow(ctx, layout)
	if err != nil {
		return 0, err
	}
	return len(entries) + len(unreadable), nil
}

// promoteRecoveryOverflow turns overflow entries back into bundles, oldest
// capture first, for as many free inventory slots as there are.
//
// It is NOT gated by retention.dryRun or the first-enable grace window, and
// that is deliberate rather than an oversight. Those gate deletion: they exist
// so a pass that has not yet earned an operator's trust reports what it would
// destroy instead of destroying it. Promotion destroys nothing recoverable —
// it writes a bundle for objects that already exist and then removes a
// metadata record whose identity the bundle now carries — and withholding it
// during a week-long grace window would leave an instance's most recent work
// at the weaker tier for exactly the week after it was captured. A dry-run
// pass still says what it promoted, so the pass remains explainable.
//
// An entry whose mirror no longer holds the ref is left alone and reported:
// the record is the only remaining evidence that work existed, and deleting
// it because its objects are gone would discard that evidence as well.
func promoteRecoveryOverflow(ctx context.Context, layout instance.Layout, setup *schedulerSetup, managers []*worktree.Manager, stdout, stderr io.Writer) error {
	entries, unreadable, err := readConfiguredRecoveryOverflow(ctx, layout)
	if err != nil {
		return err
	}
	var failures error
	for _, broken := range unreadable {
		pf(stderr, "warning: recovery overflow entry %q is unreadable: %v\n", broken.Name, broken.Err)
		failures = errors.Join(failures, fmt.Errorf("unreadable recovery overflow entry %s: %w", broken.Name, broken.Err))
	}
	if len(entries) == 0 {
		return failures
	}
	policy, _ := resolveRecoveryPolicy(layout, setup.Config)
	free, err := recoveryInventoryFreeSlots(ctx, layout, policy)
	if err != nil {
		return errors.Join(failures, err)
	}
	for _, entry := range entries {
		if free <= 0 {
			return failures
		}
		if err := ctx.Err(); err != nil {
			return errors.Join(failures, err)
		}
		promoted, err := promoteRecoveryOverflowEntry(ctx, layout, setup, managers, policy, entry.Record)
		if err != nil {
			pf(stderr, "warning: recovery overflow promotion failed run=%q ref=%q: %v\n", entry.Record.RunID, entry.Record.Ref, err)
			failures = errors.Join(failures, err)
			continue
		}
		if !promoted {
			pf(stderr, "warning: recovery overflow entry run=%q ref=%q cannot be promoted: its objects are no longer in the managed repository\n", entry.Record.RunID, entry.Record.Ref)
			continue
		}
		pf(stdout, "retention promoted kind=recovery-overflow run=%q ref=%q\n", entry.Record.RunID, entry.Record.Ref)
		free--
	}
	return failures
}

// recoveryInventoryFreeSlots counts what the inventory can still take. It
// counts unreadable reservations against the cap for the same reason the read
// model does: they occupy slots until reconciled, so a count that excluded
// them would promise capacity the next reservation does not find.
func recoveryInventoryFreeSlots(ctx context.Context, layout instance.Layout, policy instance.RecoverySnapshotConfig) (int, error) {
	limit := policy.MaxSnapshotsEffective()
	entries, unreadable, err := recovery.ReadInventoryTolerant(ctx, filepath.Join(layout.Root, "recovery"), recovery.MaxInventoryEntries)
	if err != nil {
		return 0, recoveryInventoryReadError(err, limit)
	}
	return limit - len(entries) - len(unreadable), nil
}

// promoteRecoveryOverflowEntry republishes one overflow record as a bundle
// through the ordinary publication path, so a promoted entry is byte-for-byte
// the entry a cleanup with a free slot would have written. Only once that
// bundle is durable is the overflow record removed.
func promoteRecoveryOverflowEntry(ctx context.Context, layout instance.Layout, setup *schedulerSetup, managers []*worktree.Manager, policy instance.RecoverySnapshotConfig, record recovery.Record) (bool, error) {
	url, err := recoveryRetentionCloneURL(setup.Config, record.RepositoryKey)
	if err != nil {
		return false, err
	}
	root := filepath.Join(layout.Root, "recovery")
	overflowRoot := recoveryOverflowRoot(layout)
	promoted := false
	visit := func(repositories []string) error {
		repository, err := recoveryOverflowSource(ctx, repositories, record)
		if err != nil || repository == "" {
			return err
		}
		// cleanupRoots is the source repository itself: promotion removes
		// nothing, and PublishRetainedState requires a declared set it can
		// prove the inventory sits outside of.
		if _, _, err := recovery.PublishToInventoryWithEviction(ctx, repository, root, []string{repository}, record,
			policy.MaxSnapshotsEffective(), policy.MaxArchiveBytesEffective(), nil); err != nil {
			return err
		}
		if err := recovery.DeleteOverflowEntry(overflowRoot, record); err != nil {
			return err
		}
		promoted = true
		return nil
	}
	for _, manager := range managers {
		found, err := manager.WithRecoveryRepositories(ctx, url, visit)
		if err != nil {
			return false, err
		}
		if found {
			return promoted, nil
		}
	}
	return false, fmt.Errorf("recovery overflow promotion requires an existing managed repository")
}

// recoveryOverflowSource picks the managed repository that still holds the
// pin. A record may have been captured into the mirror while a pinned clone
// never saw it, so "the first repository" is not good enough.
func recoveryOverflowSource(ctx context.Context, repositories []string, record recovery.Record) (string, error) {
	for _, repository := range repositories {
		if recovery.HasSnapshotRef(ctx, repository, record) {
			return repository, nil
		}
	}
	return "", nil
}

// readRecoveryEntryRecord reads the current record for an entry in EITHER
// tier, so selection, abandonment, viewing and restore treat an overflow
// entry as first-class rather than as a record they cannot parse. The
// retained read is tried first because it is the one that also overlays a
// renewal sidecar, which the overflow tier has no equivalent of.
func readRecoveryEntryRecord(path string) (recovery.Record, error) {
	record, err := recovery.ReadRetainedRecord(path)
	if err == nil {
		return record, nil
	}
	overflow, overflowErr := recovery.ReadOverflowRecord(path)
	if overflowErr != nil {
		return recovery.Record{}, err
	}
	return overflow, nil
}

// recoveryEntriesWithOverflow appends the overflow tier to a retained set.
// An unreadable overflow root is reported rather than silently treated as an
// empty one: a caller deciding whether recovery state exists must not be told
// "none" because a directory could not be read.
func recoveryEntriesWithOverflow(ctx context.Context, layout instance.Layout, entries []recovery.InventoryEntry) ([]recovery.InventoryEntry, error) {
	overflow, _, err := readConfiguredRecoveryOverflow(ctx, layout)
	if err != nil {
		return nil, err
	}
	return append(entries, overflow...), nil
}

// importRecoveryObjects makes a record's objects available in destination from
// whichever tier holds them: the bundle beside the record, or — for an
// overflow entry, which has no bundle — the managed repository still holding
// the pin.
func importRecoveryObjects(ctx context.Context, layout instance.Layout, cfg *instance.Config, destination, recordPath string, record recovery.Record, overflow bool, maxBytes int64) error {
	if !overflow {
		return recovery.ImportSnapshotBundle(ctx, destination, filepath.Join(filepath.Dir(recordPath), recovery.BundleFileName), record, maxBytes)
	}
	url, err := recoveryRetentionCloneURL(cfg, record.RepositoryKey)
	if err != nil {
		return err
	}
	manager, err := worktree.NewManager(layout.WorkcopiesDir())
	if err != nil {
		return err
	}
	imported := false
	found, err := manager.WithRecoveryRepositories(ctx, url, func(repositories []string) error {
		source, err := recoveryOverflowSource(ctx, repositories, record)
		if err != nil || source == "" {
			return err
		}
		if err := recovery.ImportSnapshotFromRepository(ctx, destination, source, record); err != nil {
			return err
		}
		imported = true
		return nil
	})
	if err != nil {
		return err
	}
	if !found || !imported {
		return fmt.Errorf("recovery overflow objects are no longer present in a managed repository for %s", record.Ref)
	}
	return nil
}

// retireExpiredRecoveryOverflow applies the SAME retirement rules to the
// overflow tier that retireExpiredRecovery applies to retained entries:
// explicit abandonment, a terminal owning run, and the elapsed retain floor.
// Nothing is ever discarded for sitting in this tier — an overflow entry is
// retired only for a reason that would have retired its bundle.
//
// Unlike promotion, this DOES observe the pass's dry-run decision: it deletes.
func retireExpiredRecoveryOverflow(ctx context.Context, layout instance.Layout, setup *schedulerSetup, managers []*worktree.Manager, runsByRoot map[string]string, dryRun bool, stdout, stderr io.Writer) error {
	entries, _, err := readConfiguredRecoveryOverflow(ctx, layout)
	if err != nil || len(entries) == 0 {
		return err
	}
	policy, _ := resolveRecoveryPolicy(layout, setup.Config)
	retainWindow, err := policy.RetainWindowEffective()
	if err != nil {
		return err
	}
	operatorEvents, err := journal.ReadInstanceLog(layout.SchedulerDir())
	if err != nil {
		return err
	}
	var failures error
	for _, entry := range entries {
		err := retireOverflowEntry(ctx, layout, setup, managers, runsByRoot, entry.Record, operatorEvents, retainWindow, dryRun, stdout)
		if err != nil {
			pf(stderr, "warning: recovery overflow retention failed run=%q ref=%q: %v\n", entry.Record.RunID, entry.Record.Ref, err)
			failures = errors.Join(failures, err)
		}
	}
	return failures
}

func retireOverflowEntry(ctx context.Context, layout instance.Layout, setup *schedulerSetup, managers []*worktree.Manager, runsByRoot map[string]string, record recovery.Record, operatorEvents []journal.Event, retainWindow time.Duration, dryRun bool, stdout io.Writer) error {
	_, runDir, err := recoveryRetentionOwner(record.RunID, managers, runsByRoot)
	if err != nil {
		return err
	}
	_, err = journal.WithIdleRunReader(ctx, runDir, func(reader *journal.Reader) error {
		eligible, err := recoveryRetirementEligible(reader, record, time.Now().UTC(), operatorEvents, retainWindow)
		if err != nil || !eligible {
			return err
		}
		if dryRun {
			pf(stdout, "retention candidate kind=recovery-overflow run=%q ref=%q\n", record.RunID, record.Ref)
			return nil
		}
		return retireOverflowRecord(ctx, layout, setup, managers, record)
	})
	return err
}

// retireOverflowRecord removes an overflow entry an authorized retention
// decision has retired, unpinning the ref in every managed copy first. It is
// the overflow tier's equivalent of retireSnapshotEverywhere.
func retireOverflowRecord(ctx context.Context, layout instance.Layout, setup *schedulerSetup, managers []*worktree.Manager, record recovery.Record) error {
	url, err := recoveryRetentionCloneURL(setup.Config, record.RepositoryKey)
	if err != nil {
		return err
	}
	overflowRoot := recoveryOverflowRoot(layout)
	visit := func(repositories []string) error {
		return recovery.RetireOverflow(ctx, overflowRoot, record, repositories)
	}
	for _, manager := range managers {
		found, err := manager.WithRecoveryRepositories(ctx, url, visit)
		if err != nil {
			return err
		}
		if found {
			return nil
		}
	}
	return fmt.Errorf("recovery overflow retirement requires an existing managed repository")
}
