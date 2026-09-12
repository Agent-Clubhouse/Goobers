//go:build unix

package main

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestMutationReceiptRejectsUnsafeSidecarBeforeWriting(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			target := filepath.Join(t.TempDir(), "outside")
			if err := os.WriteFile(target, []byte("untouched"), 0o600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, mutationsSidecarFile)
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink(target, path)
			case "hardlink":
				err = os.Link(target, path)
			case "fifo":
				err = unix.Mkfifo(path, 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := appendMutationFactAt(root, mutationFact{Provider: "github", Kind: "pr", ID: "9", Operation: "merge-intent"}); err == nil {
				t.Fatal("unsafe receipt write acknowledged")
			}
			data, err := os.ReadFile(target)
			if err != nil || string(data) != "untouched" {
				t.Fatalf("outside target changed: %q %v", data, err)
			}
		})
	}
}
