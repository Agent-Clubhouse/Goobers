//go:build windows

package durability

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReplaceFileSupportsLongPaths(t *testing.T) {
	directory := t.TempDir()
	for len(directory) < 280 {
		directory = filepath.Join(directory, strings.Repeat("segment", 5))
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(directory, "state.tmp")
	destination := filepath.Join(directory, "state.json")
	if err := os.WriteFile(destination, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceFile(source, destination); err != nil {
		t.Fatalf("ReplaceFile long path: %v", err)
	}
	if got, err := os.ReadFile(destination); err != nil || string(got) != "new" {
		t.Fatalf("destination = %q, %v", got, err)
	}
}
