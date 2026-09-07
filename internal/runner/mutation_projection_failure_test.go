package runner

import (
	"errors"
	"testing"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

type failingMutationJournal struct {
	executionJournal
	err     error
	appends int
	repairs int
}

func (j *failingMutationJournal) Append(journal.Event) error {
	j.appends++
	return j.err
}

func (j *failingMutationJournal) RepairAppendBoundary() error {
	j.repairs++
	return nil
}

func TestFinishTaskDispatchRejectsLostMutationProjection(t *testing.T) {
	for _, operation := range []string{"merge", "enqueue", "comment"} {
		t.Run(operation, func(t *testing.T) {
			appendErr := errors.New("journal storage unavailable")
			heartbeatErr := errors.New("heartbeat failed")
			removeErr := errors.New("workspace cleanup failed")
			jr := &failingMutationJournal{err: appendErr}
			done := make(chan error, 1)
			done <- heartbeatErr
			fact := mutationFact{Provider: "github", Kind: "pr", ID: "7", Operation: operation}
			if operation == "merge" {
				fact.MergeConfirmation = &providers.MergeConfirmation{RepositoryAPIURL: "https://api.github.com/repos/acme/web", PullID: "7", MergeSHA: "sha"}
			}
			if operation == "enqueue" {
				fact.QueueAdmission = &providers.QueueAdmission{RepositoryAPIURL: "https://api.github.com/repos/acme/web", PullID: "7", EntryID: "owned"}
			}
			err := finishTaskDispatch(jr, stageHeartbeat{stop: make(chan struct{}), done: done}, "land", 1, journal.AttemptPolicy, []mutationFact{fact, fact}, removeErr)
			for _, want := range []error{appendErr, heartbeatErr, removeErr} {
				if !errors.Is(err, want) {
					t.Errorf("finish error = %v, want cause %v", err, want)
				}
			}
			if jr.appends != 1 || jr.repairs != 1 {
				t.Errorf("appends=%d repairs=%d: must stop after first failed projection", jr.appends, jr.repairs)
			}
		})
	}
}

func TestFinishTaskDispatchMutationFailureAloneIsFatal(t *testing.T) {
	appendErr := errors.New("receipt append failed")
	jr := &failingMutationJournal{err: appendErr}
	done := make(chan error)
	close(done)
	err := finishTaskDispatch(jr, stageHeartbeat{stop: make(chan struct{}), done: done}, "land", 1, journal.AttemptPolicy, []mutationFact{{Provider: "github", Kind: "pr", ID: "7", Operation: "merge"}}, nil)
	if !errors.Is(err, appendErr) {
		t.Fatalf("finish error = %v, want receipt append failure", err)
	}
	if jr.appends != 1 || jr.repairs != 0 {
		t.Fatalf("appends=%d repairs=%d, want one append and no repair", jr.appends, jr.repairs)
	}
}
