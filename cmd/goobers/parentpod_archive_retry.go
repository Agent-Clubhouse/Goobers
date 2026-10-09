package main

import (
	"errors"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
)

// A terminal run can survive a daemon stop between run.finished and archive
// capture. Retry only under a newly acquired journal lease; the live runner's
// writer must never be borrowed by a concurrent terminal cleanup sweep.
func (r parentArchiveRestorer) retryRetirement(reader *journal.Reader, candidates []runner.ParentRetirementCandidate) error {
	missing := false
	for _, candidate := range candidates {
		missing = missing || candidate.RetirementSeq == 0
	}
	if !missing {
		return nil
	}
	phase, err := reader.Phase()
	if err != nil {
		return err
	}
	if phase == journal.PhaseRunning {
		return errors.New("parent retirement retry requires terminal ownership")
	}
	writer, _, err := journal.TryRecover(reader.Dir(), journal.WithScrubber(r.scrubber))
	if err != nil {
		return err
	}
	// retire re-reads current custody and terminal state while we own the
	// writer. Partial success is durable; a later retry captures only missing
	// branches and never replaces existing retirement authority.
	retireErr := r.retire(writer)
	return errors.Join(retireErr, writer.Close())
}
