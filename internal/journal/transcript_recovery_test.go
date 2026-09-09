package journal

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRecoverTranscriptCleanupAfterFinalCommit(t *testing.T) {
	for _, finalPresent := range []bool{true, false} {
		t.Run(map[bool]string{true: "committed-final", false: "missing-final"}[finalPresent], func(t *testing.T) {
			run, _ := newRun(t)
			capture, err := run.BeginTranscriptCapture("implement", "transcript")
			if err != nil {
				t.Fatal(err)
			}
			if err := capture.Append(TranscriptCheckpoint{Stream: "process-output/1", Data: []byte("partial\n"), Reason: "checkpoint"}); err != nil {
				t.Fatal(err)
			}
			// Reproduce a crash after the final span's durable event but before
			// RecordFinal can remove any of the capture's private blobs.
			ref, err := run.recordSpanEvent(Event{Type: EventSpanRecorded, Stage: "implement", Name: "transcript",
				Runner: map[string]any{"transcriptCaptureComplete": capture.id}}, []byte("final\n"))
			if err != nil {
				t.Fatal(err)
			}
			if err := run.Close(); err != nil {
				t.Fatal(err)
			}
			if !finalPresent {
				if err := os.Remove(filepath.Join(run.dir, ref.Path)); err != nil {
					t.Fatal(err)
				}
			}
			recovered, _, err := Recover(run.dir)
			if finalPresent {
				if err != nil {
					t.Fatal(err)
				}
				if err := recovered.Close(); err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				_ = recovered.Close()
				t.Fatal("missing final did not prevent partial cleanup")
			}
			for relative := range capture.blobs {
				_, statErr := os.Stat(filepath.Join(run.dir, relative))
				if finalPresent && !os.IsNotExist(statErr) {
					t.Fatalf("recovery retained superseded partial: %v", statErr)
				}
				if !finalPresent && statErr != nil {
					t.Fatalf("recovery removed the last transcript copy: %v", statErr)
				}
			}
		})
	}
}
