//go:build windows

package recovery

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteExistingSnapshotPathsSkipsWindowsReservedNames(t *testing.T) {
	repository := t.TempDir()
	if err := os.WriteFile(filepath.Join(repository, "preserve.txt"), []byte("work"), 0o600); err != nil {
		t.Fatal(err)
	}

	selected := []byte("NUL\x00dir/COM1.txt\x00aux.json\x00preserve.txt\x00")
	var destination bytes.Buffer
	if err := writeExistingSnapshotPaths(repository, selected, &destination); err != nil {
		t.Fatalf("classify reserved snapshot paths: %v", err)
	}
	if got, want := destination.String(), "preserve.txt\x00"; got != want {
		t.Fatalf("capturable paths = %q, want %q", got, want)
	}
}
