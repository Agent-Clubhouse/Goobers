package main

import (
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/journal"
)

// Resolve the installed plane when execution starts, after daemon composition.
// The existing runner handle owns the file lock; remote observers must never
// try to recover another writer behind that owner.
func containedParentJournalOwner(root, gaggle string) func(string, string, *journal.Run) (func(), error) {
	return func(runID, runGaggle string, run *journal.Run) (func(), error) {
		service, ok := stageGrantMinterFor(root).(*daemonCredentialService)
		if !ok || service.parentBorrowJournal == nil || runGaggle != gaggle {
			return nil, childworkflow.ErrAuthorityUnavailable
		}
		return service.parentBorrowJournal(runID, runGaggle, run)
	}
}
