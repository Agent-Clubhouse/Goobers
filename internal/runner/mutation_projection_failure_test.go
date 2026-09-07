package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

func TestStageWorkspacePreservationReleasesLeaseWithoutDeletingReceipt(t *testing.T) {
	path := t.TempDir()
	receipt := filepath.Join(path, mutationsSidecarFile)
	if err := os.WriteFile(receipt, []byte("receipt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	releases := 0
	w := &stageWorkspace{path: path, release: func() { releases++ }}
	if err := w.finishDispatch(context.Background(), true); err == nil {
		t.Fatal("preservation must report retained workspace")
	}
	data, err := os.ReadFile(receipt)
	if err != nil || string(data) != "receipt\n" || releases != 1 || w.release != nil {
		t.Fatalf("receipt=%q err=%v releases=%d", data, err, releases)
	}
	if err := w.finishDispatch(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) || releases != 1 {
		t.Fatalf("cleanup err=%v releases=%d, want removed and no double release", err, releases)
	}
}

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

func TestCompleteTaskDispatchPreservesWorkspaceOnProjectionFailure(t *testing.T) {
	appendErr := errors.New("receipt append failed")
	jr := &failingMutationJournal{err: appendErr}
	done := make(chan error)
	close(done)
	calls := 0
	err := completeTaskDispatch(jr, stageHeartbeat{stop: make(chan struct{}), done: done}, "land", 1, journal.AttemptPolicy, []mutationFact{{Provider: "github", Kind: "pr", ID: "7", Operation: "merge"}}, func(preserve bool) error {
		calls++
		if !preserve || jr.appends != 1 {
			t.Errorf("cleanup preserve=%v appends=%d, want preservation after attempted projection", preserve, jr.appends)
		}
		return nil
	})
	if !errors.Is(err, appendErr) || calls != 1 {
		t.Fatalf("error=%v cleanup calls=%d", err, calls)
	}
}

func TestCompleteTaskDispatchPersistsReceiptBeforeCleanup(t *testing.T) {
	run, err := journal.Create(t.TempDir(), journal.RunIdentity{RunID: "receipt-before-cleanup", Workflow: "landing", Gaggle: "web"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })
	done := make(chan error)
	close(done)
	calls := 0
	err = completeTaskDispatch(run, stageHeartbeat{stop: make(chan struct{}), done: done}, "land", 1, journal.AttemptPolicy, []mutationFact{{Provider: "github", Kind: "pr", ID: "7", Operation: "merge"}}, func(preserve bool) error {
		calls++
		if preserve {
			t.Error("successful projection should allow teardown")
		}
		reader, openErr := journal.OpenReadOnly(run.Dir())
		if openErr != nil {
			return openErr
		}
		events, readErr := reader.Events()
		if readErr != nil {
			return readErr
		}
		if len(events) != 2 || events[1].Type != journal.EventRefTouched {
			t.Errorf("cleanup began before receipt persisted: %+v", events)
		}
		return errors.New("cleanup failure remains nonfatal")
	})
	if err != nil || calls != 1 {
		t.Fatalf("error=%v cleanup calls=%d", err, calls)
	}
	reader, err := journal.OpenReadOnly(run.Dir())
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil || len(events) != 3 || events[2].Error == nil || events[2].Error.Code != "worktree_remove_failed" {
		t.Fatalf("cleanup warning lost: events=%+v error=%v", events, err)
	}
}
