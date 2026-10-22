package journal_test

import (
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

func fileBackendFactory(t *testing.T) backendFactory {
	t.Helper()
	return func() backendStore {
		root := t.TempDir()
		config := journal.FileBackendConfig{
			RunsDir: filepath.Join(root, "runs"), SchedulerDir: filepath.Join(root, "scheduler"),
		}
		return backendStore{Open: func() journal.Backend { return journal.NewFileBackend(config) }}
	}
}

func TestJournalBackendConformance(t *testing.T) {
	t.Run("file", func(t *testing.T) {
		if err := journalBackendConformance(fileBackendFactory(t)); err != nil {
			t.Fatal(err)
		}
	})
}
