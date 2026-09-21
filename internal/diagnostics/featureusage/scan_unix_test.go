//go:build unix

package featureusage

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"golang.org/x/sys/unix"
)

func TestScanNonRegularInputsCompleteWithinBound(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"fifo", "directory"} {
		if name == "directory" {
			if err := os.Mkdir(filepath.Join(dir, name), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		t.Run(name, func(t *testing.T) {
			done := make(chan error, 2)
			go func() { _, err := pinnedBytes(dir, journal.Ref{Path: name}, 1024); done <- err }()
			go func() {
				budget := int64(1024)
				_, err := boundedFile(filepath.Join(dir, name), 1024, &budget)
				done <- err
			}()
			for range 2 {
				select {
				case err := <-done:
					if err == nil {
						t.Fatal("nonregular input accepted")
					}
				case <-time.After(time.Second):
					t.Fatal("nonregular input blocked scanner")
				}
			}
		})
	}
}
