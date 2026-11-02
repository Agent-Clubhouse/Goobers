//go:build unix

package temporalops

import (
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/goobers/goobers/internal/workflow/ir"
)

func TestReadIRRefusesFIFOWithoutOpeningWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fifo")
	if err := unix.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- readIR(path, new(ir.Document)) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FIFO accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("FIFO blocked instead of refusing a nonregular input")
	}
}
