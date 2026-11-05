package journal_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

// assertReaderRaw is storage-independent: adapters seed these exact committed
// bytes using their fixture's storage API, then exercise both public read paths.
func assertReaderRaw(backend journal.ReaderBackend, ref journal.RunRef, raw []byte) error {
	records, err := backend.ReadEvents(context.Background(), ref, 1, 1)
	if err != nil {
		return err
	}
	if len(records) != 1 || !bytes.Equal(records[0].Raw, raw) {
		return fmt.Errorf("paged raw record changed: %+v", records)
	}
	reader, err := backend.OpenRun(context.Background(), ref)
	if err != nil {
		return err
	}
	records, err = reader.EventRecords()
	if err != nil {
		return err
	}
	if len(records) < 2 || !bytes.Equal(records[1].Raw, raw) {
		return fmt.Errorf("handle raw record changed: %+v", records)
	}
	return nil
}

const unknownReaderRecord = `{"schema":"goobers.dev/journal/event/v1","seq":2,"type":"runner.annotation","branch":0,"time":"2026-01-01T00:00:00Z", "future_field": {"kept":true},"name":"first"}`

func TestFileReaderPreservesRawUnknownFields(t *testing.T) {
	root := t.TempDir()
	run, err := journal.Create(root, journal.RunIdentity{RunID: "run-raw", Gaggle: "g", Workflow: "wf"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "run-raw", "events.jsonl")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := file.WriteString(unknownReaderRecord + "\n")
	closeErr := file.Close()
	if writeErr != nil {
		t.Fatal(writeErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	backend := journal.NewFileReaderBackend(journal.FileReaderBackendConfig{RunsDirs: []string{root}})
	if err := assertReaderRaw(backend, journal.RunRef{ID: "run-raw", ReadOnly: true}, []byte(unknownReaderRecord)); err != nil {
		t.Fatal(err)
	}
}

func TestFileReaderHandleHonorsOpeningContext(t *testing.T) {
	root := t.TempDir()
	run, err := journal.Create(root, journal.RunIdentity{RunID: "run-context"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = run.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	reader, err := journal.NewFileReaderBackend(journal.FileReaderBackendConfig{RunsDirs: []string{root}}).OpenRun(ctx, journal.RunRef{ID: "run-context"})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err = reader.Events(); !errors.Is(err, context.Canceled) {
		t.Fatalf("events after cancellation=%v", err)
	}
	if _, err = reader.State(); !errors.Is(err, context.Canceled) {
		t.Fatalf("state after cancellation=%v", err)
	}
}
