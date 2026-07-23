package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadDirectoryDistinguishesMissingFromFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	if entries, exists, err := readDirectory(missing); err != nil || exists || entries != nil {
		t.Fatalf("missing path = (%v, %v, %v), want absent without error", entries, exists, err)
	}

	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := readDirectory(file); err == nil || !exists {
		t.Fatalf("file path = (exists=%v, err=%v), want existing error", exists, err)
	}
}
