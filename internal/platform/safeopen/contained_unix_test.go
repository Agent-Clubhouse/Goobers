//go:build unix

package safeopen

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestOpenRegularInRootRejectsFIFOAndEscapingSymlink(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := unix.Mkfifo(filepath.Join(dir, "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	if f, err := OpenRegularInRoot(root, "fifo"); !errors.Is(err, ErrNotRegular) || f != nil {
		t.Fatalf("FIFO: %v, %v", f, err)
	}
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Fatal(err)
	}
	if f, err := OpenRegularInRoot(root, "escape"); err == nil || f != nil {
		t.Fatalf("escape: %v, %v", f, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "regular"), []byte("evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("regular", filepath.Join(dir, "internal")); err != nil {
		t.Fatal(err)
	}
	f, err := OpenRegularInRoot(root, "internal")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}
