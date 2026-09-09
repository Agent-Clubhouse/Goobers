package livejournal

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func TestRemoteTranscriptCheckpointAcknowledgmentAndRestart(t *testing.T) {
	w, runs := testWriter(t, WithScrubber(journal.NewPatternScrubber()))
	const runID = "checkpoint-run"
	now := time.Now()
	if _, err := w.Emit(t.Context(), openBatch(runID, now)); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("a", 32)
	open := Op{Kind: OpTranscriptCheckpoint, Key: id + "/open", Time: now,
		Checkpoint: &TranscriptCheckpointOp{Capture: id, Action: "open", Stage: "build", Name: "copilot-cli.transcript"}}
	emit := func(writer *Writer, op Op) (EmitResponse, error) {
		return writer.Emit(t.Context(), EmitRequest{RunID: runID, Gaggle: "web", Ops: []Op{op}})
	}
	if _, err := emit(w, open); err != nil {
		t.Fatal(err)
	}
	op := Op{Kind: OpTranscriptCheckpoint, Key: id + "/checkpoint/0", Time: now,
		Checkpoint: &TranscriptCheckpointOp{Capture: id, Action: "append", Stage: "build", Name: "copilot-cli.transcript",
			Stream: "process-output/1", Data: []byte("work ghp_" + strings.Repeat("A", 80) + "\n"), Reason: "checkpoint"}}
	ack, err := emit(w, op)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := emit(w, op); err != nil {
		t.Fatalf("lost-ack retry: %v", err)
	}
	events := readEvents(t, runs, runID)
	partials := 0
	reader, err := journal.OpenReadOnly(filepath.Join(runs, runID))
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Runner["partial"] != true {
			continue
		}
		partials++
		data, err := reader.SpanBytes(*event.Ref)
		if err != nil || string(data) != "work "+journal.Redacted+"\n" || event.Seq > ack.Seq {
			t.Fatalf("ack did not cover persisted scrubbed bytes: %v", err)
		}
	}
	if partials != 1 {
		t.Fatalf("retry duplicated checkpoint: count=%d", partials)
	}
	w.Close()
	restarted, err := NewWriter(func(string) (string, bool) { return runs, true })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.Close)
	if _, err := emit(restarted, op); err != nil {
		t.Fatalf("committed retry after restart: %v", err)
	}
	op.Key = id + "/checkpoint/1"
	op.Checkpoint.Offset = len(op.Checkpoint.Data)
	op.Checkpoint.Data = []byte("more\n")
	if _, err := emit(restarted, op); !errors.Is(err, ErrTranscriptSessionLost) {
		t.Fatalf("lost redaction state accepted continuation: %v", err)
	}
}

func TestRemoteTranscriptFinalRequiresBlobAndSurvivesRetry(t *testing.T) {
	data := []byte("canonical final transcript\n")
	digest := journal.Digest(data)
	spans := &fakeSpans{blobs: make(map[string][]byte)}
	w, runs := testWriter(t, WithSpanSource(spans))
	const runID = "checkpoint-final-run"
	now := time.Now()
	if _, err := w.Emit(t.Context(), openBatch(runID, now)); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("b", 32)
	send := func(writer *Writer, action, key string, request TranscriptCheckpointOp) error {
		request.Capture, request.Action, request.Stage, request.Name = id, action, "build", "copilot-cli.transcript"
		_, err := writer.Emit(t.Context(), EmitRequest{RunID: runID, Gaggle: "web", Ops: []Op{{
			Kind: OpTranscriptCheckpoint, Key: id + "/" + key, Time: now, Checkpoint: &request,
		}}})
		return err
	}
	if err := send(w, "open", "open", TranscriptCheckpointOp{}); err != nil {
		t.Fatal(err)
	}
	if err := send(w, "append", "checkpoint/0", TranscriptCheckpointOp{Stream: "process-output/1", Data: []byte("partial\n"), Reason: "checkpoint"}); err != nil {
		t.Fatal(err)
	}
	final := TranscriptCheckpointOp{FinalRef: &journal.Ref{Digest: digest, Size: int64(len(data))}}
	if err := send(w, "final", "final", final); err == nil {
		t.Fatal("missing final blob was acknowledged")
	}
	root := filepath.Join(runs, runID, "spans", "checkpoints")
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 1 {
		t.Fatalf("missing final lost partials: %v", err)
	}
	spans.blobs[digest] = data
	if err := send(w, "final", "final", final); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("final retained partials: %v", err)
	}
	w.Close()
	restarted, err := NewWriter(func(string) (string, bool) { return runs, true })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.Close)
	if err := send(restarted, "final", "final", final); err != nil {
		t.Fatalf("final retry after restart: %v", err)
	}
	count := 0
	for _, event := range readEvents(t, runs, runID) {
		if event.Type == journal.EventSpanRecorded && event.Name == "copilot-cli.transcript" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("final retry produced %d canonical spans", count)
	}
}
