//go:build linux

package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestNewRejectsUnusableBubblewrap(t *testing.T) {
	bin := t.TempDir()
	path := filepath.Join(bin, "bwrap")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 42\n"), 0o700); err != nil {
		t.Fatalf("write fake bubblewrap: %v", err)
	}
	t.Setenv("PATH", bin)

	if _, err := New(); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("New error = %v, want ErrUnavailable", err)
	}
}
