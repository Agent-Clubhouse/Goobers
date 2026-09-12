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
