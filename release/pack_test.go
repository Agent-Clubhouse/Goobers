package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestTargetNames(t *testing.T) {
	cases := []struct {
		target      Target
		version     string
		wantBinary  string
		wantArchive string
	}{
		{Target{"windows", "amd64"}, "v1.2.3", "goobers.exe", "goobers_v1.2.3_windows_amd64.zip"},
		{Target{"windows", "arm64"}, "dev", "goobers.exe", "goobers_dev_windows_arm64.zip"},
		{Target{"linux", "amd64"}, "v1.2.3", "goobers", "goobers_v1.2.3_linux_amd64.tar.gz"},
		{Target{"darwin", "arm64"}, "v0.9.0", "goobers", "goobers_v0.9.0_darwin_arm64.tar.gz"},
	}
	for _, c := range cases {
		if got := c.target.binaryName(); got != c.wantBinary {
			t.Errorf("%s binaryName() = %q, want %q", c.target, got, c.wantBinary)
		}
		if got := c.target.archiveName(c.version); got != c.wantArchive {
			t.Errorf("%s archiveName(%q) = %q, want %q", c.target, c.version, got, c.wantArchive)
		}
	}
}

// TestPackageArchiveZip is the core #655 guarantee, provable without a working
// windows cross-compile: a windows target packages to a .zip containing exactly
// goobers.exe with the fake binary's bytes.
func TestPackageArchiveZip(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "goobers.exe.windows-amd64")
	payload := []byte("\x4d\x5a fake PE binary bytes")
	if err := os.WriteFile(bin, payload, 0o755); err != nil {
		t.Fatal(err)
	}
	target := Target{"windows", "amd64"}

	archivePath, err := packageArchive(target, "v1.2.3", bin, dir)
	if err != nil {
		t.Fatalf("packageArchive: %v", err)
	}
	if base := filepath.Base(archivePath); base != "goobers_v1.2.3_windows_amd64.zip" {
		t.Fatalf("archive base = %q", base)
	}

	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	defer func() { _ = zr.Close() }()
	if len(zr.File) != 1 {
		t.Fatalf("zip has %d entries, want exactly 1 (goobers.exe)", len(zr.File))
	}
	entry := zr.File[0]
	if entry.Name != "goobers.exe" {
		t.Errorf("zip entry name = %q, want goobers.exe", entry.Name)
	}
	rc, err := entry.Open()
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(got, payload) {
		t.Errorf("zipped bytes = %q, want %q", got, payload)
	}
}

func TestPackageArchiveTarGz(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "goobers.linux-amd64")
	payload := []byte("\x7fELF fake elf binary bytes")
	if err := os.WriteFile(bin, payload, 0o755); err != nil {
		t.Fatal(err)
	}
	target := Target{"linux", "amd64"}

	archivePath, err := packageArchive(target, "v1.2.3", bin, dir)
	if err != nil {
		t.Fatalf("packageArchive: %v", err)
	}

	f, err := os.Open(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	tr := tar.NewReader(gz)
	hdr, err := tr.Next()
	if err != nil {
		t.Fatalf("tar next: %v", err)
	}
	if hdr.Name != "goobers" {
		t.Errorf("tar entry name = %q, want goobers", hdr.Name)
	}
	got, _ := io.ReadAll(tr)
	if !bytes.Equal(got, payload) {
		t.Errorf("tarred bytes = %q, want %q", got, payload)
	}
	if _, err := tr.Next(); err != io.EOF {
		t.Errorf("tar has more than one entry, want exactly 1")
	}
}

func TestChecksumsManifest(t *testing.T) {
	dir := t.TempDir()
	// Deliberately create in non-sorted order to prove the manifest sorts.
	files := map[string][]byte{
		"goobers_v1_windows_amd64.zip":  []byte("windows artifact"),
		"goobers_v1_linux_amd64.tar.gz": []byte("linux artifact"),
	}
	var paths []string
	for name, data := range files {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}

	manifest, err := checksumsManifest(paths)
	if err != nil {
		t.Fatalf("checksumsManifest: %v", err)
	}

	// Expected: two "<hex>  <basename>" lines, sorted (the sha of "linux..."
	// line sorts before "windows..." only if its hex does; we assert the exact
	// coreutils-compatible content instead of ordering assumptions).
	wantLinux := fmt.Sprintf("%x  goobers_v1_linux_amd64.tar.gz", sha256.Sum256(files["goobers_v1_linux_amd64.tar.gz"]))
	wantWin := fmt.Sprintf("%x  goobers_v1_windows_amd64.zip", sha256.Sum256(files["goobers_v1_windows_amd64.zip"]))
	if !bytes.Contains([]byte(manifest), []byte(wantLinux)) {
		t.Errorf("manifest missing linux line %q\ngot:\n%s", wantLinux, manifest)
	}
	if !bytes.Contains([]byte(manifest), []byte(wantWin)) {
		t.Errorf("manifest missing windows line %q\ngot:\n%s", wantWin, manifest)
	}
	// coreutils format uses two spaces between hash and filename.
	if !bytes.Contains([]byte(manifest), []byte("  goobers_")) {
		t.Errorf("manifest not in `sha256sum -c` two-space format:\n%s", manifest)
	}
}

func TestParseTargets(t *testing.T) {
	if got, _ := parseTargets(""); len(got) != len(DefaultTargets) {
		t.Errorf("empty targets = %v, want DefaultTargets", got)
	}
	got, err := parseTargets("windows/amd64, linux/arm64")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].String() != "windows/amd64" || got[1].String() != "linux/arm64" {
		t.Errorf("parseTargets = %v", got)
	}
	if _, err := parseTargets("bogus"); err == nil {
		t.Error("parseTargets(\"bogus\") should error (no slash)")
	}
}

// TestDefaultMatrixExcludesWindowsArm64 pins the #655 AC decision: windows/arm64
// is deliberately NOT a published target (never-run binary), while windows/amd64
// is.
func TestDefaultMatrixExcludesWindowsArm64(t *testing.T) {
	var hasWinAmd64, hasWinArm64 bool
	for _, tgt := range DefaultTargets {
		switch tgt.String() {
		case "windows/amd64":
			hasWinAmd64 = true
		case "windows/arm64":
			hasWinArm64 = true
		}
	}
	if !hasWinAmd64 {
		t.Error("DefaultTargets must include windows/amd64 (#655)")
	}
	if hasWinArm64 {
		t.Error("DefaultTargets must NOT include windows/arm64 (deferred: never-run binary)")
	}
}
