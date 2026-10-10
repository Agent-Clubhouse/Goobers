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
	switch cause := err.(type) {
	case interface{ Unwrap() []error }:
		children := cause.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !onlyCancellationRetention(child) {
				return false
			}
		}
		return true
	case interface{ Unwrap() error }:
		return onlyCancellationRetention(cause.Unwrap())
	default:
		return err == invoke.ErrChildCustodyPending || err == worktree.ErrCleanupDeferred || err == journal.ErrRecoveryBusy
	}
}
