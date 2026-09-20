package main

import (
	"context"
	"fmt"
	"time"

	"github.com/goobers/goobers/internal/instance"
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
	// The cap does not bound this read: an inventory already holding more
	// entries than it refused the read outright, which stopped telemetry
	// retention entirely on exactly the instances under the most pressure
	// (#5354). Fail-closed is preserved where it matters — a reservation no
	// scan can interpret may still name a run that owns live recovery state,
	// so the guard refuses rather than pruning journals it cannot rule out.
	entries, unreadable, _, err := observeRecoveryInventory(ctx, layout)
	if err != nil {
		return nil, noop, err
	}
	if len(unreadable) > 0 {
		return nil, noop, fmt.Errorf(
			"recovery inventory holds %d unreadable reservation(s); refusing to prune run journals that may still own recovery state",
			len(unreadable))
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
