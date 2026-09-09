//go:build !windows

package journal

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTranscriptCleanupRefusesRedirectedParent(t *testing.T) {
	run, _ := newRun(t)
	capture, err := run.BeginTranscriptCapture("implement", "transcript")
	if err != nil {
		t.Fatal(err)
	}
	if err := capture.Append(TranscriptCheckpoint{Stream: "process-output/1", Data: []byte("partial\n"), Reason: "checkpoint"}); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(run.dir, dirSpans, "checkpoints")
	if err := os.Rename(parent, parent+".saved"); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Mkdir(filepath.Join(outside, capture.id), 0o700); err != nil {
		t.Fatal(err)
	}
	var sentinel string
	for relative := range capture.blobs {
		sentinel = filepath.Join(outside, capture.id, filepath.Base(relative))
		if err := os.WriteFile(sentinel, []byte("unrelated content"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, parent); err != nil {
		t.Fatal(err)
	}
	if _, err := capture.RecordFinal("", []byte("final\n")); err == nil {
		t.Fatal("redirected cleanup accepted")
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "unrelated content" {
		t.Fatalf("cleanup changed data outside the journal: %v", err)
	}
}
