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

func TestIntegrationRetainPreparedHonorsCapacityWithoutRecapturing(t *testing.T) {
	testdep.Require(t, "git")
	for _, overflow := range []bool{false, true} {
		name := "refuse"
		if overflow {
			name = "overflow"
		}
		t.Run(name, func(t *testing.T) {
			repository := t.TempDir()
			recoveryTestGit(t, repository, "init", "--initial-branch=main")
			recoveryTestGit(t, repository, "commit", "--allow-empty", "-m", "base")
			work := filepath.Join(repository, "work.txt")
			if err := os.WriteFile(work, []byte("first capture\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			template := storageTestRecord()
			request := RetentionRequest{
				Repository: repository, RepositoryKey: template.RepositoryKey, RunID: template.RunID,
				BaseRef: "main", IdentityTime: template.CreatedAt, RetainUntil: template.RetainUntil,
				InventoryRoot: t.TempDir(), CleanupRoots: []string{repository}, MaxSnapshots: 1, MaxArchiveBytes: 1 << 20,
			}
			log := retentionJournalFunc(func(journal.Event) error { return nil })
			first, _, err := Retain(t.Context(), request, log)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(work, []byte("interrupted capture\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			prepared, err := PrepareRecord(t.Context(), repository, request.RepositoryKey, request.RunID, request.BaseRef, request.IdentityTime, request.RetainUntil)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(work, []byte("later unowned edits\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			evictions, observations := 0, 0
			request.EvictFull = func(context.Context, string, int) (bool, error) {
				evictions++
				return false, nil
			}
			request.AcknowledgeArchive = func(context.Context, Record, string) error {
				t.Fatal("overflow or refused publication has no archive to acknowledge")
				return nil
			}
			if overflow {
				request.OverflowRoot = t.TempDir()
			}
			log = retentionJournalFunc(func(event journal.Event) error {
				observations++
				if event.Runner["recoveryOverflow"] != true || event.Runner["recoveryCapture"] != true || event.Runner["recoveryRef"] != prepared.Ref {
					t.Fatal("wrong custody acknowledgement", event)
				}
				return nil
			})
			got, path, err := RetainPrepared(t.Context(), request, prepared, log)
			if !overflow {
				if !errors.Is(err, ErrInventoryFull) || got != (Record{}) || path != "" || observations != 0 {
					t.Fatal("full inventory did not refuse without acknowledgement", got, path, err)
				}
			} else {
				if err != nil || got != prepared || observations != 1 {
					t.Fatal("overflow did not retain exact preparation", got, err)
				}
				stored, err := ReadOverflowRecord(path)
				if err != nil || stored != prepared {
					t.Fatal("overflow record changed", stored, err)
				}
				if content := recoveryTestGit(t, repository, "show", got.Ref+":work.txt"); content != "interrupted capture" {
					t.Fatal("publication recaptured later edits", content)
				}
			}
			if evictions == 0 {
				t.Fatal("publication bypassed configured eviction")
			}
			entries, err := ReadInventory(t.Context(), request.InventoryRoot, 10)
			if err != nil || len(entries) != 1 || entries[0].Record != first {
				t.Fatal("publication changed existing inventory", entries, err)
			}
			if data, err := os.ReadFile(work); err != nil || string(data) != "later unowned edits\n" {
				t.Fatal("publication changed live workspace", string(data), err)
			}
		})
	}
}
