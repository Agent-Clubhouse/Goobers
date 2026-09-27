//go:build unix

package history

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestSnapshotRejectsSymlinksAndNonRegularInputs(t *testing.T) {
	for _, name := range []string{snapshotName, lockName} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			outside := filepath.Join(t.TempDir(), "private")
			if err := os.WriteFile(outside, []byte("untouched"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(dir, name)); err != nil {
				t.Fatal(err)
			}
			if store, err := Open(context.Background(), dir, nil); err == nil {
				_ = store.Close()
				t.Fatal("symlink accepted")
			}
			data, err := os.ReadFile(outside)
			if err != nil || string(data) != "untouched" {
				t.Fatal("outside target altered", err)
			}
		})
	}
	dir := t.TempDir()
	if err := unix.Mkfifo(filepath.Join(dir, snapshotName), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(dir); err == nil {
		t.Fatal("FIFO accepted")
	}
	if store, err := Open(context.Background(), dir, nil); err == nil {
		_ = store.Close()
		t.Fatal("FIFO accepted by writer")
	}
}
