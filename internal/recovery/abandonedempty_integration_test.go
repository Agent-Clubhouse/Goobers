//go:build integration

package recovery

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

// A worktree preparation that fails during its initial checkout still leaves a
// branch whose tree matches its parent. Capturing those wedged a live
// inventory for the full 30-day retain floor, after which every unrelated run
// whose own preparation failed could no longer complete its durable handoff
// (#4994). None of them may consume a slot, however many arrive at once.
func TestIntegrationEmptyAbandonedPreparationsConsumeNoInventorySlot(t *testing.T) {
	testdep.Require(t, "git")
	repository, inventory := t.TempDir(), t.TempDir()
	recoveryTestGit(t, repository, "init", "--initial-branch=receiving")
	recoveryTestGit(t, repository, "commit", "--allow-empty", "-m", "base")
	template := storageTestRecord()
	const abandoned = 8
	branches := make([]string, abandoned)
	for i := range branches {
		runID := fmt.Sprintf("run-empty-%d", i)
		branch, err := PreparedRestoreBranch(runID)
		if err != nil {
			t.Fatal(err)
		}
		branches[i] = branch
		recoveryTestGit(t, repository, "checkout", "-b", branch)
		// The preparation commit exists; no implementation was ever authored.
		recoveryTestGit(t, repository, "commit", "--allow-empty", "-m", "prepared")
		recoveryTestGit(t, repository, "checkout", "receiving")
	}
	log := retentionJournalFunc(func(journal.Event) error {
		t.Error("empty preparation was journaled as a recovery capture")
		return nil
	})
	// One slot total: a single wrongful capture would both take it and, by
	// the retain floor, keep the genuine capture below from ever landing.
	newRequest := func(runID string) RetentionRequest {
		return RetentionRequest{Repository: repository, RepositoryKey: template.RepositoryKey, RunID: runID,
			IdentityTime: template.CreatedAt, RetainUntil: template.RetainUntil, InventoryRoot: inventory,
			CleanupRoots: []string{repository}, MaxSnapshots: 1, MaxArchiveBytes: 1 << 20, SkipEmpty: true}
	}
	var wait sync.WaitGroup
	errs := make([]error, abandoned)
	for i := range branches {
		wait.Add(1)
		go func() {
			defer wait.Done()
			errs[i] = RetainAbandonedPreparation(context.Background(), newRequest(fmt.Sprintf("run-empty-%d", i)), log)
		}()
	}
	wait.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("abandoned preparation %d: %v", i, err)
		}
	}
	entries, err := ReadInventory(context.Background(), inventory, 1)
	if err != nil || len(entries) != 0 {
		t.Fatalf("empty preparations consumed inventory slots: %d %v", len(entries), err)
	}
	if surviving := recoveryTestGit(t, repository, "for-each-ref", "--format=%(refname)", "refs/heads/goobers/recovery-resume/"); surviving != "" {
		t.Fatalf("abandoned preparations survived cleanup: %q", surviving)
	}
	// The remaining slot is still available to a preparation worth keeping.
	captured := 0
	log = retentionJournalFunc(func(event journal.Event) error {
		if event.Runner["recoveryCapture"] != true {
			t.Error("handoff lost capture ordering")
		}
		captured++
		return nil
	})
	branch, err := PreparedRestoreBranch("run-authored")
	if err != nil {
		t.Fatal(err)
	}
	recoveryTestGit(t, repository, "checkout", "-b", branch)
	writeRestoreFixture(t, repository, "implementation", "retained work")
	recoveryTestGit(t, repository, "add", "implementation")
	recoveryTestGit(t, repository, "commit", "-m", "prepared")
	recoveryTestGit(t, repository, "checkout", "receiving")
	if err := RetainAbandonedPreparation(context.Background(), newRequest("run-authored"), log); err != nil {
		t.Fatalf("authored preparation refused after empty burst: %v", err)
	}
	if captured != 1 {
		t.Fatalf("authored preparation captures = %d, want 1", captured)
	}
	entries, err = ReadInventory(context.Background(), inventory, 1)
	if err != nil || len(entries) != 1 {
		t.Fatalf("authored preparation was not retained: %d %v", len(entries), err)
	}
	if entries[0].Record.RunID != "run-authored" {
		t.Fatalf("retained the wrong preparation: %+v", entries[0].Record)
	}
}

// Without SkipEmpty the caller has not opted into discarding anything, so an
// empty preparation is still captured rather than silently deleted.
func TestIntegrationEmptyAbandonedPreparationRetainedWithoutSkipEmpty(t *testing.T) {
	testdep.Require(t, "git")
	repository, inventory := t.TempDir(), t.TempDir()
	recoveryTestGit(t, repository, "init", "--initial-branch=receiving")
	recoveryTestGit(t, repository, "commit", "--allow-empty", "-m", "base")
	template := storageTestRecord()
	branch, err := PreparedRestoreBranch(template.RunID)
	if err != nil {
		t.Fatal(err)
	}
	recoveryTestGit(t, repository, "checkout", "-b", branch)
	recoveryTestGit(t, repository, "commit", "--allow-empty", "-m", "prepared")
	recoveryTestGit(t, repository, "checkout", "receiving")
	request := RetentionRequest{Repository: repository, RepositoryKey: template.RepositoryKey, RunID: template.RunID,
		IdentityTime: template.CreatedAt, RetainUntil: template.RetainUntil, InventoryRoot: inventory,
		CleanupRoots: []string{repository}, MaxSnapshots: 1, MaxArchiveBytes: 1 << 20}
	if err := RetainAbandonedPreparation(context.Background(), request, retentionJournalFunc(func(journal.Event) error { return nil })); err != nil {
		t.Fatal(err)
	}
	entries, err := ReadInventory(context.Background(), inventory, 1)
	if err != nil || len(entries) != 1 {
		t.Fatalf("opt-out capture = %d entries, %v", len(entries), err)
	}
}
