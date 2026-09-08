package mutationsidecar

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestReadBoundsAndFileType(t *testing.T) {
	root := t.TempDir()
	if _, err := Read(root); !errors.Is(err, os.ErrNotExist) || !os.IsNotExist(err) {
		t.Fatalf("missing receipt: %v", err)
	}
	path := filepath.Join(root, "mutations.jsonl")
	data := []byte("{\"operation\":\"merge\"}\n")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := Read(root); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("receipt changed: %q %v", got, err)
	}
	if err := os.Truncate(path, MaxBytes+1); err != nil {
		t.Fatal(err)
	}
	if got, err := Read(root); err == nil || got != nil {
		t.Fatalf("oversized receipt accepted or partially returned: %d %v", len(got), err)
	}
	other := t.TempDir()
	if err := os.Mkdir(filepath.Join(other, "mutations.jsonl"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(other); err == nil {
		t.Fatal("directory accepted")
	}
}

func TestReadBoundsDiagnosticExpansion(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "mutations.jsonl")
	data := bytes.Repeat([]byte("x\n"), MaxLines)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := Read(root); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("exact line bound rejected: %d %v", len(got), err)
	}
	if err := os.WriteFile(path, append(data, 'x'), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := Read(root); err == nil || got != nil {
		t.Fatalf("unterminated extra line bypassed bound: %d %v", len(got), err)
	}
}
