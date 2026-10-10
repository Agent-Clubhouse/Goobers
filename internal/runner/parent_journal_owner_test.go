package runner

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

func TestParentJournalOwnerUsesPinnedOptInAndExistingWriter(t *testing.T) {
	for _, optedIn := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "parent"}[optedIn], func(t *testing.T) {
			machine := childEnabledMachine(t)
			if !optedIn {
				definition := machine.Def
				definition.Spec.Tasks[0].ChildWorkflows = nil
				var err error
				machine, err = workflow.Compile(definition, workflow.WithPreviewFeatures(true))
				if err != nil {
					t.Fatal(err)
				}
			}
			runs := t.TempDir()
			newPinnedDefinitionRun(t, runs, "parent-owner", machine)
			run, _, err := journal.TryRecover(filepath.Join(runs, "parent-owner"))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = run.Close() }()
			var borrowed, released bool
			runner := &Runner{cfg: Config{BorrowParentJournal: func(runID, gaggle string, existing *journal.Run) (func(), error) {
				if existing != run || runID != "parent-owner" || gaggle != "acme-web" {
					t.Fatal("borrowed foreign journal")
				}
				borrowed = true
				if err := existing.Append(journal.Event{Type: journal.EventRunnerAnnotation, Reason: "same-owner observation"}); err != nil {
					return nil, err
				}
				return func() { released = true }, nil
			}}}
			release, err := runner.borrowExecutionJournal(run)
			if err != nil {
				t.Fatal(err)
			}
			if borrowed != optedIn || released {
				t.Fatal("incorrect journal ownership lifetime", borrowed, released)
			}
			release()
			if released != optedIn {
				t.Fatal("borrowed writer did not release")
			}
		})
	}
}

func TestParentJournalOwnerRefusesUnavailableRelease(t *testing.T) {
	runs := t.TempDir()
	newPinnedDefinitionRun(t, runs, "parent-owner", childEnabledMachine(t))
	run, _, err := journal.TryRecover(filepath.Join(runs, "parent-owner"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = run.Close() }()
	for _, failure := range []error{nil, errors.New("writer unavailable")} {
		runner := &Runner{cfg: Config{BorrowParentJournal: func(string, string, *journal.Run) (func(), error) { return nil, failure }}}
		if _, err := runner.borrowExecutionJournal(run); err == nil {
			t.Fatal("missing journal release admitted")
		}
	}
}
