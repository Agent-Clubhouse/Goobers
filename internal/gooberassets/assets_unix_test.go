//go:build unix

package gooberassets

import (
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// TestLoadAndValidateRejectSpecialFile uses a FIFO — a unix-only special file —
// to prove the loader rejects a non-regular, non-directory asset. It lives in a
// unix-tagged file so the package's test build stays clean under
// GOOS=windows go vet (#1090's compile gate); the equivalent windows special
// file is out of scope until the Windows CI job lands.
func TestLoadAndValidateRejectSpecialFile(t *testing.T) {
	source := t.TempDir()
	if err := unix.Mkfifo(filepath.Join(source, "stream"), 0o600); err != nil {
		t.Skipf("FIFO unsupported: %v", err)
	}
	if _, err := Load(source); err == nil {
		t.Fatal("Load accepted a FIFO asset")
	}
	if err := Validate(source); err == nil {
		t.Fatal("Validate accepted a FIFO asset")
	}
}
