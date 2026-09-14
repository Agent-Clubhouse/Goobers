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

// TestNormalizeDirModeStripsSetgidAndSticky uses synthetic fs.FileMode values
// so it needs no setgid-capable filesystem: it fails before the fix (the bits
// pass through untouched) and passes after. Directory setgid and sticky bits
// are placement metadata a volume attaches (for example Kubernetes sets
// setgid on an fsGroup-managed emptyDir so descendants inherit its group),
// not content, so they must be cleared before a mode is recorded.
func TestNormalizeDirModeStripsSetgidAndSticky(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   fs.FileMode
		want fs.FileMode
	}{
		{"setgid directory", fs.ModeDir | fs.ModeSetgid | 0o750, fs.ModeDir | 0o750},
		{"sticky directory", fs.ModeDir | fs.ModeSticky | 0o750, fs.ModeDir | 0o750},
		{"setgid and sticky directory", fs.ModeDir | fs.ModeSetgid | fs.ModeSticky | 0o750, fs.ModeDir | 0o750},
		{"plain directory unaffected", fs.ModeDir | 0o750, fs.ModeDir | 0o750},
		{"regular file setuid untouched", fs.ModeSetuid | 0o750, fs.ModeSetuid | 0o750},
	} {
		if got := normalizeDirMode(tc.in); got != tc.want {
			t.Errorf("%s: normalizeDirMode(%v) = %v, want %v", tc.name, tc.in, got, tc.want)
		}
	}
}

// TestApplySourceModesAcceptsNormalizedSetgidDirectory shows the source-mode
// validator, fed the same synthetic setgid root mode a scan would have
// already normalized, accepts it as a complete bundle description. It exists
// to pin the contract at the applySourceModes seam without a real filesystem.
func TestApplySourceModesAcceptsNormalizedSetgidDirectory(t *testing.T) {
	bundle := &Bundle{entries: []entry{{path: "file", mode: 0o640}}}
	// A raw setgid root mode, not yet normalized, must still be rejected here:
	// normalization is expected to happen at the scan seam, not this one.
	if err := bundle.applySourceModes(map[string]fs.FileMode{
		".":    fs.ModeDir | fs.ModeSetgid | 0o750,
		"file": 0o640,
	}); err == nil {
		t.Fatal("accepted an unnormalized setgid root mode")
	}
	if err := bundle.applySourceModes(map[string]fs.FileMode{
		".":    normalizeDirMode(fs.ModeDir | fs.ModeSetgid | 0o750),
		"file": 0o640,
	}); err != nil {
		t.Fatalf("rejected a normalized setgid root mode: %v", err)
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
