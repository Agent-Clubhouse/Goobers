package runner

import (
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// OwnedJournalRecorder is the bounded writer surface supplied by the runner to
// trusted execution factories. Branch adapters retain branch attribution for
// host custody markers and artifacts without changing the run's default branch.
type OwnedJournalRecorder interface {
	ArtifactRecorder
	Append(journal.Event) error
	Dir() string
	RecordArtifactWithIntegrity(string, []byte, apiv1.Integrity) (journal.Ref, error)
	RecordArtifactBoundedWithIntegrity(string, []byte, apiv1.Integrity, int) (journal.Ref, error)
}

// OwnedJournalScope accepts only concrete writers created by this runner. An
// arbitrary recorder implementing the same methods does not establish custody.
func OwnedJournalScope(rec ArtifactRecorder) (OwnedJournalRecorder, int, error) {
	switch writer := rec.(type) {
	case *journal.Run:
		if writer != nil {
			return writer, 0, nil
		}
	case *branchJournal:
		if writer != nil && writer.run != nil && writer.branch > 0 && writer.branch <= 128 {
			return writer, writer.branch, nil
		}
	}
	return nil, 0, fmt.Errorf("runner: execution requires its owned journal writer")
}

// OwnedBranchRecorder restores exact branch attribution while a host recovery
// coordinator exclusively owns the run writer. It does not acquire ownership.
func OwnedBranchRecorder(run *journal.Run, branch int) (OwnedJournalRecorder, error) {
	if run == nil || branch < 0 || branch > 128 {
		return nil, fmt.Errorf("runner: invalid owned journal branch")
	}
	if branch == 0 {
		return run, nil
	}
	return &branchJournal{run: run, branch: branch}, nil
}
