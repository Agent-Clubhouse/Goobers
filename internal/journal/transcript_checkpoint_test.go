package journal

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTranscriptCheckpointDurableScrubbedDeltas(t *testing.T) {
	registry, scrubber := DefaultScrubber()
	registry.Register([]byte("registered-secret"))
	root := t.TempDir()
	run, err := Create(root, testIdentity(), nil, WithScrubber(scrubber))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })
	capture, err := run.BeginTranscriptCapture("implement", "copilot-cli.transcript")
	if err != nil {
		t.Fatal(err)
	}
	parts := []string{"first registered-sec", "ret\nsecond ", "ghp_" + strings.Repeat("A", 80) + "\n"}
	offset := 0
	for i, part := range parts {
		reason := "checkpoint"
		if i == len(parts)-1 {
			reason = "canceled"
		}
		if err := capture.Append(TranscriptCheckpoint{Stream: "process-output/1", Offset: offset, Data: []byte(part), Reason: reason}); err != nil {
			t.Fatal(err)
		}
		offset += len(part)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenRead(filepath.Join(root, testIdentity().RunID))
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	var got []byte
	count := 0
	for _, event := range events {
		if event.Type != EventSpanRecorded {
			continue
		}
		if event.Runner["partial"] != true || !strings.Contains(event.Ref.Path, "/checkpoints/"+capture.id+"/") {
			t.Fatal("checkpoint lacks partial identity or private storage")
		}
		data, err := reader.SpanBytes(*event.Ref)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, data...)
		count++
	}
	if count != len(parts) || !bytes.Equal(got, scrubber.Scrub([]byte(strings.Join(parts, "")))) {
		t.Fatal("durable deltas did not reconstruct the scrubbed transcript")
	}
}

func TestTranscriptCheckpointRejectsDiscontinuityPermanently(t *testing.T) {
	run, _ := newRun(t)
	capture, err := run.BeginTranscriptCapture("implement", "transcript")
	if err != nil {
		t.Fatal(err)
	}
	bad := TranscriptCheckpoint{Stream: "process-output/1", Offset: 1, Data: []byte("gap"), Reason: "checkpoint"}
	if err := capture.Append(bad); err == nil {
		t.Fatal("accepted gap")
	}
	bad.Offset = 0
	if err := capture.Append(bad); err == nil || capture.count != 0 {
		t.Fatal("failed capture resumed")
	}
}

func TestTranscriptCheckpointFinalRetiresOnlyPrivateBlobs(t *testing.T) {
	run, _ := newRun(t)
	capture, err := run.BeginTranscriptCapture("implement", "transcript")
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("same content\n")
	if err := capture.Append(TranscriptCheckpoint{Stream: "process-output/1", Data: data, Reason: "process-exit"}); err != nil {
		t.Fatal(err)
	}
	ref, err := capture.RecordFinal("", data)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(run.dir, ref.Path)); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("cleanup removed final blob: %v", err)
	}
	if _, err := os.Stat(filepath.Join(run.dir, dirSpans, "checkpoints", capture.id)); !os.IsNotExist(err) {
		t.Fatalf("partial remains: %v", err)
	}
	if _, err := capture.RecordFinal("", data); err != nil {
		t.Fatalf("idempotent completion: %v", err)
	}
	if err := capture.Append(TranscriptCheckpoint{}); err == nil {
		t.Fatal("completed capture accepted more bytes")
	}
	events, _, err := readEvents(filepath.Join(run.dir, fileEvents))
	if err != nil {
		t.Fatal(err)
	}
	finals := 0
	for _, event := range events {
		if event.Type == EventSpanRecorded && event.Name == "transcript" {
			finals++
		}
	}
	if finals != 1 {
		t.Fatalf("final span count=%d", finals)
	}
}
