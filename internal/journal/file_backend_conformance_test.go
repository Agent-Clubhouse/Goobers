package journal_test

import (
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

func fileBackendFactory(t *testing.T) backendFactory {
	t.Helper()
	return func() journal.Backend {
		root := t.TempDir()
		return journal.NewFileBackend(journal.FileBackendConfig{
			RunsDir: filepath.Join(root, "runs"), SchedulerDir: filepath.Join(root, "scheduler"),
		})
	}
}

func TestJournalBackendConformance(t *testing.T) {
	t.Run("file", func(t *testing.T) {
		if err := journalBackendConformance(fileBackendFactory(t)); err != nil {
			t.Fatal(err)
		}
	})
}
