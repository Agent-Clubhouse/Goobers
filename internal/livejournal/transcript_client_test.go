package livejournal

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/journal"
)

func TestWorkflowTranscriptAdoptionRequiresFinalBytes(t *testing.T) {
	w, runs := testWriter(t)
	const runID = "adoption-custody"
	if _, err := w.Emit(t.Context(), openBatch(runID, time.Now())); err != nil {
		t.Fatal(err)
	}
	session, err := (TranscriptTransport{RunID: runID, Gaggle: "web", Emitter: w}).OpenTranscriptCheckpoint(
		runID+":build", "copilot-cli.transcript", journal.NewPatternScrubber())
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("final transcript\n")
	ref, err := session.RecordFinal("", data)
	if err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(runs, runID, ref.Path)
	if err := os.Remove(filename); err != nil {
		t.Fatal(err)
	}
	request := EmitRequest{RunID: runID, Gaggle: "web", Ops: []Op{{Kind: OpSpan, Key: "workflow-final",
		Span: &SpanOp{Stage: "build", Name: "build.transcript", Ref: ref}}}}
	if _, err := w.Emit(t.Context(), request); err == nil {
		t.Fatal("completion marker acknowledged missing final bytes")
	}
	if err := os.WriteFile(filename, data, 0600); err != nil {
		t.Fatal(err)
	}
	ack, err := w.Emit(t.Context(), request)
	if err != nil || ack.Applied != 1 || ack.Deduplicated != 0 {
		t.Fatalf("missing bytes poisoned the workflow retry key: %+v, %v", ack, err)
	}
	finals := 0
	for _, event := range readEvents(t, runs, runID) {
		if event.Type == journal.EventSpanRecorded && event.Runner["partial"] != true {
			finals++
		}
	}
	if finals != 1 {
		t.Fatalf("adoption duplicated final transcript: %d", finals)
	}
}

type transcriptAcknowledgmentDropper struct {
	writer  *Writer
	dropped bool
}

func (d *transcriptAcknowledgmentDropper) Emit(ctx context.Context, request EmitRequest) (EmitResponse, error) {
	response, err := d.writer.Emit(ctx, request)
	if err == nil && !d.dropped && request.Ops[0].Checkpoint.Action == "final" {
		d.dropped = true
		return EmitResponse{}, errors.New("simulated lost final acknowledgment")
	}
	return response, err
}

func TestTranscriptFinalRetryCannotChangeUnacknowledgedContent(t *testing.T) {
	w, runs := testWriter(t)
	const runID = "lost-final-ack"
	if _, err := w.Emit(t.Context(), openBatch(runID, time.Now())); err != nil {
		t.Fatal(err)
	}
	dropper := &transcriptAcknowledgmentDropper{writer: w}
	session, err := (TranscriptTransport{RunID: runID, Gaggle: "web", Emitter: dropper}).OpenTranscriptCheckpoint(
		"build", "copilot-cli.transcript", journal.NewPatternScrubber())
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("committed final\n")
	if _, err := session.RecordFinal("schema-a", data); err == nil || !dropper.dropped {
		t.Fatal("lost acknowledgment was not simulated")
	}
	if _, err := session.RecordFinal("schema-a", []byte("different final\n")); err == nil {
		t.Fatal("changed content reused an uncertain commit key")
	}
	if _, err := session.RecordFinal("schema-b", data); err == nil {
		t.Fatal("changed schema reused an uncertain commit key")
	}
	ref, err := session.RecordFinal("schema-a", data)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenReadOnly(filepath.Join(runs, runID))
	if err != nil {
		t.Fatal(err)
	}
	got, err := reader.SpanBytes(ref)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("acknowledged ref does not match committed bytes: %v", err)
	}
	finals := 0
	for _, event := range readEvents(t, runs, runID) {
		if event.Type == journal.EventSpanRecorded && event.Runner["partial"] != true {
			finals++
		}
	}
	if finals != 1 {
		t.Fatalf("retry produced %d final transcripts", finals)
	}
}

func TestTranscriptTransportScrubsChunksAndCommitsFinal(t *testing.T) {
	blobs, err := blobstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, runs := testWriter(t, WithSpanSource(blobs), WithScrubber(journal.NewPatternScrubber()))
	const runID = "transport-run"
	if _, err := w.Emit(t.Context(), openBatch(runID, time.Now())); err != nil {
		t.Fatal(err)
	}
	transport := TranscriptTransport{RunID: runID, Gaggle: "web", Emitter: w, Blobs: blobs}
	registry, scrubber := journal.DefaultScrubber()
	registry.Register([]byte("registered-secret"))
	session, err := transport.OpenTranscriptCheckpoint("build", "copilot-cli.transcript", scrubber)
	if err != nil {
		t.Fatal(err)
	}
	parts := []string{"output: registered-sec", "ret\n" + strings.Repeat("ordinary output\n", 80000)}
	offset := 0
	for _, part := range parts {
		if err := session.Append(journal.TranscriptCheckpoint{Stream: "process-output/1", Offset: offset,
			Data: []byte(part), Reason: "checkpoint"}); err != nil {
			t.Fatal(err)
		}
		offset += len(part)
	}
	reader, err := journal.OpenReadOnly(filepath.Join(runs, runID))
	if err != nil {
		t.Fatal(err)
	}
	var partial []byte
	for _, event := range readEvents(t, runs, runID) {
		if event.Runner["partial"] == true {
			data, err := reader.SpanBytes(*event.Ref)
			if err != nil {
				t.Fatal(err)
			}
			partial = append(partial, data...)
		}
	}
	want := scrubber.Scrub([]byte(strings.Join(parts, "")))
	if !bytes.Equal(partial, want) {
		t.Fatal("remote checkpoints lost bytes or leaked a split registered secret")
	}
	ref, err := session.RecordFinal("", want)
	if err != nil {
		t.Fatal(err)
	}
	if data, err := reader.SpanBytes(ref); err != nil || !bytes.Equal(data, want) {
		t.Fatalf("remote final ref unavailable: %v", err)
	}
	if _, err := session.RecordFinal("", want); err != nil {
		t.Fatalf("final retry: %v", err)
	}
}

func TestTranscriptTransportWithoutBlobStore(t *testing.T) {
	w, runs := testWriter(t)
	const runID = "inline-final-run"
	if _, err := w.Emit(t.Context(), openBatch(runID, time.Now())); err != nil {
		t.Fatal(err)
	}
	transport := TranscriptTransport{RunID: runID, Gaggle: "web", Emitter: w}
	session, err := transport.OpenTranscriptCheckpoint("build", "copilot-cli.transcript", journal.NewPatternScrubber())
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Append(journal.TranscriptCheckpoint{Stream: "process-output/1", Data: []byte("partial\n"), Reason: "checkpoint"}); err != nil {
		t.Fatal(err)
	}
	data := []byte(strings.Repeat("complete transcript\n", 250000))
	ref, err := session.RecordFinal("", data)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := journal.OpenReadOnly(filepath.Join(runs, runID))
	if err != nil {
		t.Fatal(err)
	}
	got, err := reader.SpanBytes(ref)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("inline final unavailable: %v", err)
	}
	if _, err := session.RecordFinal("", data); err != nil {
		t.Fatal(err)
	}
	adopt := func(writer *Writer, key string) {
		t.Helper()
		request := EmitRequest{RunID: runID, Gaggle: "web", Ops: []Op{{Kind: OpSpan, Key: key,
			Span: &SpanOp{Stage: "build", Name: "build.transcript", Ref: ref}}}}
		for range 2 {
			if _, err := writer.Emit(t.Context(), request); err != nil {
				t.Fatalf("workflow span adoption: %v", err)
			}
		}
	}
	adopt(w, "workflow-span")
	w.Close()
	restarted, err := NewWriter(func(gaggle string) (string, bool) { return runs, gaggle == "web" })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.Close)
	adopt(restarted, "workflow-span")
	adopt(restarted, "workflow-span-after-restart")
	finals, adoptions := 0, 0
	for _, event := range readEvents(t, runs, runID) {
		if event.Type == journal.EventError {
			t.Fatal("workflow reported unavailable bytes already held in the journal")
		}
		if event.Type == journal.EventSpanRecorded && event.Runner["partial"] != true {
			finals++
		}
		if event.Runner["transcriptCaptureAdopted"] != nil {
			adoptions++
		}
	}
	if finals != 1 || adoptions != 2 {
		t.Fatalf("workflow duplicated finalized transcript or retry: finals=%d adoptions=%d", finals, adoptions)
	}
}
