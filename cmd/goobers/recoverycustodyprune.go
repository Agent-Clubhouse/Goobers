package main

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/telemetry/retention"
)

// openRecoveryCustodyPruneGuard refuses to let telemetry retention delete a
// run's journal while that run still owns a live (non-retired) recovery
// record (#4824): every recovery consumer — retirement (recoveryexpiry.go),
// selection (recoveryselect.go), restore (recoveryrestore.go) — re-opens the
// owning run journal, and once it is gone none of them can ever select,
// restore, or retire the record again, permanently stranding its inventory
// slot. retireExpiredRecovery already retires a record on its own schedule
// once it actually expires; this guard only makes telemetry retention wait
// for that, rather than racing ahead of it. Mirrors openTriggerPruneGuard's
// shape: the inventory is read once per pass (not once per candidate), and a
// dry run opens nothing.
func openRecoveryCustodyPruneGuard(layout instance.Layout, dryRun bool) (func(retention.Result) error, func(), error) {
	noop := func() {}
	if dryRun {
		return nil, noop, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	entries, err := recovery.ReadInventory(ctx, filepath.Join(layout.Root, "recovery"), 128)
	if err != nil {
		return nil, noop, err
	}
	owners := make(map[string]bool, len(entries))
	for _, entry := range entries {
		owners[entry.Record.RunID] = true
	}
	if len(owners) == 0 {
		return nil, noop, nil
	}
	return func(candidate retention.Result) error {
		if owners[candidate.RunID] {
			return fmt.Errorf(
				"run %s still owns a live recovery snapshot; refusing to delete its journal until the snapshot is retired or expires",
				candidate.RunID)
		}
		return nil
	}, noop, nil
}

// combineBeforeDeleteGuards runs every non-nil guard in order, stopping at
// the first error (#4824) — retention.Prune already treats a BeforeDelete
// error as "preserve this journal, abort the rest of this pass" (see
// pruneOne's doc comment), so composing guards this way keeps that same
// fail-closed contract regardless of how many independent custody checks
// exist.
func combineBeforeDeleteGuards(guards ...func(retention.Result) error) func(retention.Result) error {
	active := make([]func(retention.Result) error, 0, len(guards))
	for _, guard := range guards {
		if guard != nil {
			active = append(active, guard)
		}
	}
	if len(active) == 0 {
		return nil
	}
	return func(candidate retention.Result) error {
		for _, guard := range active {
			if err := guard(candidate); err != nil {
				return err
			}
		}
		return nil
	}
}
