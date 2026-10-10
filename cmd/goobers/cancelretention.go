package main

import (
	"errors"

	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/worktree"
)

// A terminal parent can still own child results and its held checkout. Report
// accepted cancellation, never completed family shutdown, for that expected
// retention. The original retirement diagnostics remain in the run journal.
// Every joined cause must be expected; a separate I/O or cleanup failure still
// surfaces as an error even when child custody is pending alongside it.
func deferredFamilyCancellation(phase journal.RunPhase, err error) bool {
	return phase == journal.PhaseAborted && errors.Is(err, invoke.ErrChildCustodyPending) && onlyCancellationRetention(err)
}

func onlyCancellationRetention(err error) bool {
	// Find the first branching node through any unary wrappers. Every branch
	// is then inspected, so errors.As cannot hide a sibling failure.
	var joined interface{ Unwrap() []error }
	if errors.As(err, &joined) {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !onlyCancellationRetention(child) {
				return false
			}
		}
		return true
	}
	var wrapped interface{ Unwrap() error }
	if errors.As(err, &wrapped) {
		return onlyCancellationRetention(wrapped.Unwrap())
	}
	return errors.Is(err, invoke.ErrChildCustodyPending) || errors.Is(err, worktree.ErrCleanupDeferred) || errors.Is(err, journal.ErrRecoveryBusy)
}
