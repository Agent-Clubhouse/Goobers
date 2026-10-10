package runner

import (
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

func TestOwnedJournalScopeRejectsAbsentAndForgedOwnership(t *testing.T) {
	for _, rec := range []ArtifactRecorder{nil, (*journal.Run)(nil), (*branchJournal)(nil), &branchJournal{branch: 1}, &branchJournal{run: &journal.Run{}, branch: 129}} {
		if _, _, err := OwnedJournalScope(rec); err == nil {
			t.Fatal("invalid ownership admitted")
		}
	}
	run := &journal.Run{}
	for _, branch := range []int{0, 1, 128} {
		rec, err := OwnedBranchRecorder(run, branch)
		if err != nil {
			t.Fatal(err)
		}
		owned, got, err := OwnedJournalScope(rec)
		if err != nil || got != branch || owned != rec {
			t.Fatal("ownership changed", got, err)
		}
	}
	for _, branch := range []int{-1, 129} {
		if _, err := OwnedBranchRecorder(run, branch); err == nil {
			t.Fatal("invalid recovery branch admitted")
		}
	}
	if _, err := OwnedBranchRecorder(nil, 0); err == nil {
		t.Fatal("nil recovery owner admitted")
	}
}
