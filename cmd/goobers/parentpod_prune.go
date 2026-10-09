package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/telemetry/retention"
)

// A failed terminal capture still needs the journal's contract, original source
// policy, and private parent blobs. Once archived, the recovery inventory guard
// owns that provenance through the snapshot's retention lifetime.
func preserveParentWorkspaceJournal(candidate retention.Result) error {
	reader, err := journal.OpenReadOnly(candidate.RunDir)
	if errors.Is(err, os.ErrNotExist) && candidate.Reason == "interrupted" {
		return nil // Removal already passed its guard and deleted journal identity.
	}
	if err != nil {
		return err
	}
	candidates, err := runner.ParentRetirementCandidates(reader)
	if errors.Is(err, runner.ErrParentReturnPending) {
		return fmt.Errorf("%w: parent writer return is still pending: %w", retention.ErrCustodyHeld, err)
	}
	if err != nil {
		return err
	}
	for _, parent := range candidates {
		if parent.RetirementSeq == 0 {
			return fmt.Errorf("%w: run %s still requires parent workspace archival", retention.ErrCustodyHeld, candidate.RunID)
		}
	}
	return nil
}
