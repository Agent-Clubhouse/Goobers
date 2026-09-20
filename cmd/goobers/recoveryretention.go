package main

import (
	"context"
	"io"
	"path/filepath"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/recovery"
)

// Invoked by the same startup/periodic retention sweep and exclusion gate as
// retained worktrees. Only already-committed retirements are handled here;
// active recovery records require their separate run/policy eligibility check.
// dryRun is the caller's already-resolved decision, which folds in the
// operator's retention.dryRun and the first-enable grace window (#4253) — this
// reaper must not re-derive it, or a retirement would be deleted during a
// window that is still only reporting worktrees.
func reapConfiguredRecovery(ctx context.Context, layout instance.Layout, cfg instance.RetentionConfig, dryRun bool, stdout, stderr io.Writer) error {
	if !cfg.EnabledEffective() && !cfg.DryRun {
		return nil
	}
	policy, _ := resolveRecoveryPolicy(layout, &instance.Config{Retention: cfg})
	limit := policy.MaxSnapshotsEffective()
	results, err := recovery.ReapRetired(ctx, filepath.Join(layout.Root, "recovery"), limit, !dryRun)
	err = recoveryInventoryReadError(err, limit)
	for _, result := range results {
		switch {
		case result.Err != nil:
			pf(stderr, "warning: recovery retirement cleanup failed path=%q: %v\n", result.Path, result.Err)
		case result.DryRun:
			pf(stdout, "retention candidate kind=retired-recovery path=%q\n", result.Path)
		case result.Deleted:
			pf(stdout, "retention deleted kind=retired-recovery path=%q\n", result.Path)
		}
	}
	// Background sweeps discard text output. Return failures so their error
	// reporter still makes deletion failures observable.
	return err
}

// reconcileIncompleteRecovery reclaims reservation directories a crashed
// publish left without a record.json. They hold no recoverable identity, yet
// they count toward maxSnapshots forever and make the strict inventory read
// fail closed, so an instance that accumulates them stops being able to clean
// up worktrees at all (#5177). Running it from the retention pass covers both
// entry points that pass has: the startup sweep deferred until API readiness
// and the periodic ticker.
//
// operatorDryRun is the operator's explicit retention.dryRun alone, not the
// pass's combined decision: an incomplete reservation holds nothing an
// operator could review, so the first-enable grace window does not apply to
// it (#5354) — an already-wedged instance heals in full on its first pass
// after upgrade rather than waiting out the window one refused publish at a
// time. graceActive is passed through only so a successful deletion can note
// when the grace window would otherwise have held it.
func reconcileIncompleteRecovery(ctx context.Context, layout instance.Layout, cfg instance.RetentionConfig, operatorDryRun, graceActive bool, stdout, stderr io.Writer) error {
	if !cfg.EnabledEffective() && !cfg.DryRun {
		return nil
	}
	policy, _ := resolveRecoveryPolicy(layout, &instance.Config{Retention: cfg})
	limit := policy.MaxSnapshotsEffective()
	root := filepath.Join(layout.Root, "recovery")
	results, err := recovery.ReconcileIncompleteReservations(ctx, root, limit, recovery.IncompleteReservationGrace, !operatorDryRun)
	err = recoveryInventoryReadError(err, limit)
	for _, result := range results {
		reportIncompleteRecovery(result, graceActive, stdout, stderr)
	}
	return err
}

// A reservation younger than the grace window may be a publish still writing
// its archive, so it is neither a candidate nor a failure: it is reported as
// the capacity it still legitimately holds.
func reportIncompleteRecovery(result recovery.ReconcileResult, graceActive bool, stdout, stderr io.Writer) {
	switch {
	case result.Err != nil:
		pf(stderr, "warning: incomplete recovery reservation cleanup failed path=%q: %v\n", result.Path, result.Err)
	case !result.Stale:
		pf(stdout, "retention retained kind=incomplete-recovery-reservation path=%q reason=publish-may-be-in-flight\n", result.Path)
	case result.DryRun:
		pf(stdout, "retention candidate kind=incomplete-recovery-reservation path=%q\n", result.Path)
	case result.Deleted:
		if graceActive {
			pf(stdout, "retention deleted kind=incomplete-recovery-reservation path=%q (grace window does not apply: no recoverable content)\n", result.Path)
			return
		}
		pf(stdout, "retention deleted kind=incomplete-recovery-reservation path=%q\n", result.Path)
	}
}
