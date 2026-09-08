//go:build !windows

package mutationsidecar

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestReadRefusesSymlinkAndFIFO(t *testing.T) {
	for _, kind := range []string{"symlink", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "mutations.jsonl")
			var err error
			if kind == "fifo" {
				err = unix.Mkfifo(path, 0600)
			} else {
				target := filepath.Join(t.TempDir(), "secret")
				if err := os.WriteFile(target, []byte("PRIVATE"), 0600); err != nil {
					t.Fatal(err)
				}
				err = os.Symlink(target, path)
			}
			if err != nil {
				t.Fatal(err)
			}
			if data, err := Read(root); err == nil || data != nil {
				t.Fatalf("unsafe file read: %q %v", data, err)
			}
		})
	}
}
