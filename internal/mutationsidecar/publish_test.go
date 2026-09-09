package mutationsidecar

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
)

type lostReceiptAck struct{ writer *livejournal.Writer }

func (e lostReceiptAck) Emit(ctx context.Context, req livejournal.EmitRequest) (livejournal.EmitResponse, error) {
	response, err := e.writer.Emit(ctx, req)
	if err == nil {
		err = errors.New("response lost after durable journal append")
	}
	return response, err
}

func TestReceiptPublicationRecoversLostAckAfterHostRestart(t *testing.T) {
	runs, workspace := t.TempDir(), t.TempDir()
	const owner = "source-run"
	run, err := journal.Create(runs, journal.RunIdentity{RunID: owner, Gaggle: "gaggle", Workflow: "implementation", WorkflowVersion: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"receiptId":"receipt-1","provider":"github","kind":"pr","id":"42","operation":"merge"}`)
	path := filepath.Join(workspace, "mutations.jsonl")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	newWriter := func() *livejournal.Writer {
		writer, err := livejournal.NewWriter(func(gaggle string) (string, bool) { return runs, gaggle == "gaggle" })
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(writer.Close)
		return writer
	}
	writer := newWriter()
	scrubber := journal.NewPatternScrubber()
	if err := PublishBeforeCleanup(t.Context(), workspace, "source-land", owner, "gaggle", scrubber, lostReceiptAck{writer}); err == nil {
		t.Fatal("lost ACK authorized cleanup")
	}
	writer.Close()
	writer = newWriter()
	if err := PublishBeforeCleanup(t.Context(), workspace, "source-land", owner, "gaggle", scrubber, writer); err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenReadOnly(filepath.Join(runs, owner))
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range events {
		if event.Runner["mutationReceiptId"] == "receipt-1" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("retry retained %d copies of one receipt", count)
	}
	// Local cleanup recognizes the same proof without borrowing the live
	// writer lock or trusting only the transport ACK.
	if err := RecoverBeforeCleanup(t.Context(), workspace, "source-land", owner, filepath.Join(runs, owner)); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != string(data) {
		t.Fatalf("publication modified source: %q %v", got, err)
	}
}
