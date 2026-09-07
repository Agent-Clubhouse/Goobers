//go:build unix

package blobstore

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestDirBoundedReadRejectsEscapesAndSpecialFiles(t *testing.T) {
	for _, kind := range []string{"file-link", "parent-link", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			store, err := NewDir(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			data := []byte("outside evidence with a matching digest")
			digest := digestOf(data)
			filePath, err := store.pathFor(digest)
			if err != nil {
				t.Fatal(err)
			}
			outside := t.TempDir()
			if err := os.WriteFile(filepath.Join(outside, filepath.Base(filePath)), data, 0o600); err != nil {
				t.Fatal(err)
			}
			if kind == "parent-link" {
				if err := os.MkdirAll(filepath.Dir(filepath.Dir(filePath)), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Dir(filePath)); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.MkdirAll(filepath.Dir(filePath), 0o700); err != nil {
					t.Fatal(err)
				}
				if kind == "fifo" {
					if err := unix.Mkfifo(filePath, 0o600); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Symlink(filepath.Join(outside, filepath.Base(filePath)), filePath); err != nil {
					t.Fatal(err)
				}
			}
			if got, err := store.GetBounded(context.Background(), digest, 1024); got != nil || err == nil {
				t.Fatalf("unsafe store object returned data: %q, %v", got, err)
			}
		})
	}
}
