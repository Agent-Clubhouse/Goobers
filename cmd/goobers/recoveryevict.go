package main

import (
	"context"
	"path/filepath"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/worktree"
)

// recoveryEvictFunc returns an eviction hook (#4823) that retires the first
// reclaimable inventory entry it finds for repository key when the inventory
// is full, so an in-progress cleanup is not refused while reclaimable
// capacity exists. It only considers entries for key: a cross-repository
// entry still needs that repository's own managed clone to verify landing,
// which this single cleanup call does not have; the periodic sweep
// (retireExpiredRecovery) still reclaims those. It never returns an error for
// an individual candidate that turns out ineligible or ambiguous — a single
// bad entry must not block eviction of a later, genuinely reclaimable one, or
// fail the cleanup that is waiting on capacity.
//
// manager must be the SAME manager whose repository lock for key is already
// held by the caller (true of every wiring in this package: a cleanup-guard
// callback, or a request handler inside WithRecoveryMirror) — this hook uses
// WithRecoveryRepositoriesLocked, never WithRecoveryRepositories, precisely
// because re-acquiring that lock here would deadlock on it.
func recoveryEvictFunc(layout instance.Layout, cfg *instance.Config, manager *worktree.Manager, key string) recovery.EvictFunc {
	return func(ctx context.Context, root string, limit int) (bool, error) {
		entries, err := recovery.ReadInventory(ctx, root, limit)
		if err != nil {
			return false, err
		}
		for _, entry := range entries {
			if entry.Record.RepositoryKey != key {
				continue
			}
			if retired, _ := tryRetireLandedEntry(ctx, root, layout, cfg, manager, entry); retired {
				return true, nil
			}
		}
		return false, nil
	}
}

// tryRetireLandedEntry retires one inventory entry purely on landing proof —
// deliberately independent of the periodic sweep's dry-run, first-enable
// grace window, and 30-day retain-until floor (#4823 AC4): those gate a
// background policy decision made with time to spare, while this runs only
// when a cleanup is already blocked on capacity.
func tryRetireLandedEntry(ctx context.Context, root string, layout instance.Layout, cfg *instance.Config, manager *worktree.Manager, entry recovery.InventoryEntry) (bool, error) {
	record, err := recovery.ReadRetainedRecord(entry.RecordPath)
	if err != nil {
		return false, err
	}
	runDir := filepath.Join(layout.RunsDir(), record.RunID)
	var retired bool
	_, err = journal.WithIdleRunReader(ctx, runDir, func(reader *journal.Reader) error {
		identity, err := reader.Identity()
		if err != nil || identity.RunID != record.RunID {
			return err
		}
		phase, err := reader.PhaseBounded(ctx)
		if err != nil || !terminalRunPhase(phase) {
			return err
		}
		project, err := recoveryConfiguredProject(cfg, record.RepositoryKey)
		if err != nil {
			return err
		}
		route, err := recoveryLandingRoute(project)
		if err != nil {
			return err
		}
		landed, err := recoveryLandingHeads(ctx, layout.RunsDir(), record, route)
		if err != nil || len(landed) == 0 {
			return err
		}
		url, err := recoveryRetentionCloneURL(cfg, record.RepositoryKey)
		if err != nil {
			return err
		}
		_, err = manager.WithRecoveryRepositoriesLocked(ctx, url, func(repositories []string) error {
			verified, err := verifyRecoveryLandingRepositories(ctx, repositories, record, landed, cfg.Retention.RecoveryEffective().MaxArchiveBytesEffective())
			if err != nil || !verified {
				return err
			}
			_, err = recovery.RetireSnapshot(ctx, root, record, func(current recovery.Record) error {
				for _, repository := range repositories {
					if err := recovery.DeleteSnapshotRef(ctx, repository, current); err != nil {
						return err
					}
				}
				return nil
			})
			if err == nil {
				retired = true
			}
			return err
		})
		return err
	})
	return retired, err
}
