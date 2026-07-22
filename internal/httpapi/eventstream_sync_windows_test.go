//go:build windows

package httpapi

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/platform/durability"
)

func TestSyncObservedFileDoesNotBlockAtomicReplacement(t *testing.T) {
	directory := t.TempDir()
	state := filepath.Join(directory, "state.json")
	temp := state + ".tmp"
	if err := os.WriteFile(state, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	reader, err := os.Open(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := syncObservedFile(reader, state); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(temp, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := durability.ReplaceFile(temp, state); err != nil {
		t.Fatalf("ReplaceFile after observer sync: %v", err)
	}
}
