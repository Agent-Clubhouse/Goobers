//go:build integration

package recovery

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

type retentionJournalFunc func(journal.Event) error

func (f retentionJournalFunc) Append(event journal.Event) error { return f(event) }

func TestIntegrationRetentionRequiresJournalAcknowledgementAfterArchive(t *testing.T) {
	testdep.Require(t, "git")
	repository, inventory := t.TempDir(), t.TempDir()
	recoveryTestGit(t, repository, "init", "--initial-branch=main")
	recoveryTestGit(t, repository, "commit", "--allow-empty", "-m", "base")
	if err := os.WriteFile(filepath.Join(repository, "dirty.txt"), []byte("retained work"), 0o600); err != nil {
		t.Fatal(err)
	}
	template := storageTestRecord()
	request := RetentionRequest{Repository: repository, RepositoryKey: template.RepositoryKey, RunID: template.RunID, BaseRef: "main", IdentityTime: template.CreatedAt, RetainUntil: template.RetainUntil, InventoryRoot: inventory, CleanupRoots: []string{repository}, MaxSnapshots: 1, MaxArchiveBytes: 1 << 20}
	blocked := errors.New("journal unavailable")
	var publishedPath string
	var observations []journal.Event
	log := retentionJournalFunc(func(event journal.Event) error {
		entries, err := os.ReadDir(inventory)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if entry.IsDir() {
				publishedPath = filepath.Join(inventory, entry.Name(), RecordFileName)
			}
		}
		stored, err := ReadRecord(publishedPath)
		if err != nil || stored.Ref != event.Runner["recoveryRef"] {
			t.Fatalf("journal preceded bound record publication: %+v %v", stored, err)
		}
		if info, err := os.Stat(filepath.Join(filepath.Dir(publishedPath), BundleFileName)); err != nil || info.Size() != stored.ArchiveBytes {
			t.Fatalf("journal preceded archive publication: %v", err)
		}
		observations = append(observations, event)
		return blocked
	})
	got, path, err := Retain(context.Background(), request, log)
	if !errors.Is(err, blocked) || got != (Record{}) || path != "" {
		t.Fatalf("failed journal acknowledged cleanup: %+v %q %v", got, path, err)
	}
	if _, err := ReadRecord(publishedPath); err != nil {
		t.Fatal("journal failure discarded the published recovery record")
	}
	blocked = nil
	got, path, err = Retain(context.Background(), request, log)
	if err != nil || path != publishedPath || got.Ref != observations[0].Runner["recoveryRef"] || len(observations) != 2 {
		t.Fatalf("retry failed to acknowledge same snapshot: %+v %q %v", got, path, err)
	}
}

// TestIntegrationRetainSkipsCustodyForAGenuinelyEmptyDiff pins #5103's
// acceptance criterion: a stage whose pod made no commits and left no dirty
// files must skip custody cleanly, with no journal write and no acknowledge
// call at all — not merely "no error". SkipEmpty only ever gets to run this
// check once PrepareRecord can resolve the base in the first place, so this
// also exercises resolution against a base reachable only as a
// remote-tracking ref, the shape a mode-3 pod's checkout leaves behind on an
// already-existing run branch (#5103).
func TestIntegrationRetainSkipsCustodyForAGenuinelyEmptyDiff(t *testing.T) {
	testdep.Require(t, "git")
	origin := t.TempDir()
	recoveryTestGit(t, origin, "init", "--initial-branch=main")
	recoveryTestGit(t, origin, "commit", "--allow-empty", "-m", "base")
	recoveryTestGit(t, origin, "checkout", "-b", "run-branch")

	// The shape checkoutRepoWorkspace's "already exists" arm leaves: a
	// single-branch clone of the run branch, no local "main" at all, only the
	// remote-tracking ref recovery custody now fetches for exactly this.
	parent := t.TempDir()
	recoveryTestGit(t, parent, "clone", "--quiet", "--branch", "run-branch", origin, "checkout")
	repository := filepath.Join(parent, "checkout")
	recoveryTestGit(t, repository, "fetch", "--quiet", "origin", "main:refs/remotes/origin/main")
	if got := recoveryTestGit(t, repository, "branch", "--list", "main"); got != "" {
		t.Fatalf("test fixture unexpectedly carries a local main branch: %q", got)
	}

	inventory := t.TempDir()
	template := storageTestRecord()
	request := RetentionRequest{
		Repository: repository, RepositoryKey: template.RepositoryKey, RunID: template.RunID,
		BaseRef: "refs/remotes/origin/main", IdentityTime: template.CreatedAt, RetainUntil: template.RetainUntil,
		InventoryRoot: inventory, CleanupRoots: []string{repository}, MaxSnapshots: 1, MaxArchiveBytes: 1 << 20,
		SkipEmpty: true,
		AcknowledgeArchive: func(context.Context, Record, string) error {
			t.Fatal("an empty diff still attempted custody upload")
			return nil
		},
	}
	log := retentionJournalFunc(func(journal.Event) error {
		t.Fatal("an empty diff still journaled a retention publication")
		return nil
	})
	got, path, err := Retain(context.Background(), request, log)
	if err != nil || got != (Record{}) || path != "" {
		t.Fatalf("empty diff was not skipped cleanly: %+v %q %v", got, path, err)
	}
	entries, err := os.ReadDir(inventory)
	if err != nil || len(entries) != 0 {
		t.Fatalf("empty diff consumed an inventory slot: %v %v", entries, err)
	}
}
