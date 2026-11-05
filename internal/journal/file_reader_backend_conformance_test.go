package journal_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

func fileReaderBackendFactory(t *testing.T) readerBackendFactory {
	t.Helper()
	return func() (journal.ReaderBackend, error) {
		root := t.TempDir()
		config := journal.FileBackendConfig{
			RunsDir: filepath.Join(root, "runs"), SchedulerDir: filepath.Join(root, "scheduler"),
		}
		writer := newCleanupBackend(t, journal.NewFileBackend(config))
		if err := seedReaderBackend(writer); err != nil {
			return nil, err
		}
		// The storage adapter injects a future envelope field after retirement.
		// A writer that only knows today's Event cannot produce that field.
		path := filepath.Join(config.RunsDir, "run-reader", "events.jsonl")
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		lines := bytes.Split(data, []byte("\n"))
		lines[1] = []byte(unknownReaderRecord)
		if err := os.WriteFile(path, bytes.Join(lines, []byte("\n")), 0600); err != nil {
			return nil, err
		}
		return journal.NewFileReaderBackend(journal.FileReaderBackendConfig{
			RunsDirs: []string{config.RunsDir}, SchedulerDir: config.SchedulerDir,
		}), nil
	}
}

func TestJournalReaderBackendConformanceFile(t *testing.T) {
	if err := journalReaderBackendConformance(fileReaderBackendFactory(t)); err != nil {
		t.Fatal(err)
	}
}
