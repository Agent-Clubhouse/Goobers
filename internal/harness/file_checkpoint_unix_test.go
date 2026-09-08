//go:build unix

package harness

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestFileTranscriptCheckpointRejectsSymlinksAndFIFO(t *testing.T) {
	dir := t.TempDir()
	root, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("do not capture"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(dir, "leaf")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "parent")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(dir, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"leaf", "parent/secret", "pipe"} {
		state := fileTranscriptCheckpoint{root: root, path: path, sink: func(TranscriptDelta) error { t.Fatal("unsafe file reached sink"); return nil }}
		if err := state.capture("checkpoint"); err == nil {
			t.Fatalf("accepted unsafe file %q", path)
		}
	}
}
