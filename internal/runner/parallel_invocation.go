package runner

import (
	"errors"

	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
)

// An uncertain physical writer must retain its branch cursor until recovery
// joins that writer. Settling it as failed would make a safe resume impossible.
func parallelInvocationFailed(err error, result *parallelBranchResult) bool {
	if err == nil {
		return false
	}
	result.status, result.err = journal.BranchFailed, err
	if errors.Is(err, errChildWaitDrain) || errors.Is(err, invoke.ErrChildCustodyPending) {
		result.status, result.paused = journal.BranchCancelled, true
		if errors.Is(err, errChildWaitDrain) && !errors.Is(err, invoke.ErrChildCustodyPending) {
			result.err = nil
		}
	}
	return true
}
