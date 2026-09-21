package main

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestExtractArchiveRejectsUnsafeEntries(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind byte
		size int64
	}{
		{"../outside", tar.TypeReg, 0},
		{"/absolute", tar.TypeReg, 0},
		{"link", tar.TypeSymlink, 0},
		{"hardlink", tar.TypeLink, 0},
		{"oversized", tar.TypeReg, 33 << 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var data bytes.Buffer
			writer := tar.NewWriter(&data)
			if err := writer.WriteHeader(&tar.Header{Name: tc.name, Typeflag: tc.kind, Size: tc.size, Linkname: "outside"}); err != nil {
				t.Fatal(err)
			}
			// Oversized input is rejected from its header without reading its payload.
			if err := extractArchive(bytes.NewReader(data.Bytes()), t.TempDir()); err == nil {
				t.Fatal("unsafe archive accepted")
			}
		})
	}
}

func TestExtractArchivePreservesRegularFixture(t *testing.T) {
	var data bytes.Buffer
	writer := tar.NewWriter(&data)
	if err := writer.WriteHeader(&tar.Header{Name: "internal/fixture.txt", Typeflag: tar.TypeReg, Size: 3}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("old")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := extractArchive(bytes.NewReader(data.Bytes()), root); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(root, "internal", "fixture.txt"))
	if err != nil || string(got) != "old" {
		t.Fatalf("fixture=%q err=%v", got, err)
	}
}

func TestSchemaSnapshotDoesNotCreateMissingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.db")
	if _, err := schemaSnapshot(path); err == nil {
		t.Fatal("missing fixture accepted")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("read-only schema query created database: %v", err)
	}
}
