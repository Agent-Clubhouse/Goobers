package gooberassets

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestSourceModesPreserveFingerprintAcrossNativeModeChanges(t *testing.T) {
	source := filepath.Join(t.TempDir(), "assets")
	writeAsset(t, source, "scripts/run.sh", "#!/bin/sh\necho hello\n", 0o751)
	original, err := Load(source)
	if err != nil {
		t.Fatal(err)
	}
	modes := map[string]fs.FileMode{".": original.rootMode}
	for _, entry := range original.entries {
		modes[filepath.ToSlash(entry.path)] = entry.mode
	}
	if err := WriteSourceModes(source, modes); err != nil {
		t.Fatal(err)
	}
	// Model a target filesystem which cannot represent the source modes.
	if err := os.Chmod(filepath.Join(source, "scripts/run.sh"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(source, 0o700); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(source)
	if err != nil || loaded.Fingerprint() != original.Fingerprint() {
		t.Fatalf("source identity changed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(source, "scripts/run.sh"), []byte("modified"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(source); err == nil {
		t.Fatal("accepted changed content with stale source metadata")
	}
}

func TestSourceModesRejectSpecialBitsAndIncompleteBundle(t *testing.T) {
	for _, modes := range []map[string]fs.FileMode{
		{".": fs.ModeDir | 0o755},
		{".": fs.ModeDir | 0o755, "file": fs.ModeSetuid | 0o755},
		{".": fs.ModeDir | 0o755, "file": fs.ModeSymlink | 0o755},
	} {
		source := filepath.Join(t.TempDir(), "assets")
		writeAsset(t, source, "file", "content", 0o644)
		if err := WriteSourceModes(source, modes); err == nil {
			t.Fatal("accepted incomplete or privileged metadata")
		}
	}
}
