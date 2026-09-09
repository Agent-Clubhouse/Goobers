package livejournal

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/journal"
)

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
