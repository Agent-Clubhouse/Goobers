//go:build cloudintegration

package journal_test

import (
	"errors"
	"os"
	"testing"

	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/journal"
)

// reviewer-2 rev-8167: an unresolved token minted by a RECOVERY must not be
// adopted by CreateRun as if it had created the run: CreateRun on an existing
// run with history should not succeed, and must not skip recovery's partial
// transcript cleanup.
func TestPostgresCreateRunDoesNotAdoptRecoveryMint(t *testing.T) {
	b, _, fault := mintFaultBackend(t, false)
	h, owner, err := b.CreateRun(t.Context(), mintRequest())
	if err != nil {
		t.Fatal(err)
	}
	capture, err := b.BeginTranscript(t.Context(), h, owner, 0, "stage", "transcript")
	if err != nil {
		t.Fatal(err)
	}
	if err := capture.Append(journal.TranscriptCheckpoint{Stream: "process-output/1", Data: []byte("partial\n"), Reason: "checkpoint"}); err != nil {
		t.Fatal(err)
	}
	records, err := b.ReadEvents(t.Context(), journal.RunRef{ID: "run-mint"}, 1, 0)
	if err != nil || len(records) != 1 || records[0].Event.Ref == nil {
		t.Fatalf("partial: %v %v", records, err)
	}
	partial := *records[0].Event.Ref
	id := records[0].Event.Runner["transcriptCapture"]
	if _, err := b.AppendBatch(t.Context(), h, owner, []journal.Event{{Type: journal.EventRunnerAnnotation, Runner: map[string]any{"transcriptCaptureComplete": id}}}); err != nil {
		t.Fatal(err)
	}
	if err := b.Retire(t.Context(), h, owner); err != nil {
		t.Fatal(err)
	}
	fault.armed.Store(true)
	fault.failRead.Store(true)
	_, _, _, err = b.RecoverRun(t.Context(), journal.RunKey{Gaggle: "g", RunID: "run-mint"}, journal.RunWriterOptions{})
	fault.finish(t)
	if !errors.Is(err, lease.ErrCommitUnknown) {
		t.Fatalf("precondition: want an unresolved recovery takeover: %v", err)
	}
	// A failed create must leave the unresolved recovery available, and must
	// not publish fresh-create notifications for a run with existing history.
	request := mintRequest()
	notified := false
	request.Options.AppendObserver = func(string, uint64) { notified = true }
	ch, cowner, cerr := b.CreateRun(t.Context(), request)
	if !errors.Is(cerr, os.ErrExist) || ch != "" || cowner != "" || notified {
		t.Fatalf("CreateRun adopted recovery mint: handle=%q owner set=%v err=%v notified=%v", ch, cowner != "", cerr, notified)
	}
	h, owner, report, err := b.RecoverRun(t.Context(), journal.RunKey{Gaggle: "g", RunID: "run-mint"}, journal.RunWriterOptions{})
	if err != nil || len(report.Events) != 3 {
		t.Fatalf("recovery proof/history lost after refused create: events=%d err=%v", len(report.Events), err)
	}
	if _, err := b.ReadBlob(t.Context(), h, partial); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovery did not clean the partial transcript: %v", err)
	}
	assertMintAppend(t, b, h, owner)
}
