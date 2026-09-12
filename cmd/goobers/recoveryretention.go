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
	results, err := recovery.ReapRetired(ctx, filepath.Join(layout.Root, "recovery"), 128, !dryRun)
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
