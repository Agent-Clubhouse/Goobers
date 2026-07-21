package main

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("", "  ", "x", "y"); got != "x" {
		t.Errorf("firstNonEmpty = %q, want x", got)
	}
	if got := firstNonEmpty("", ""); got != "" {
		t.Errorf("firstNonEmpty(empties) = %q, want empty", got)
	}
}

func TestParseFlagsExplicit(t *testing.T) {
	// Explicit values bypass the git-derived defaults, so this is hermetic.
	opts, err := parseFlags([]string{
		"-version", "v9.9.9", "-commit", "abc123", "-date", "2026-01-02T03:04:05Z",
		"-targets", "windows/amd64", "-output", "out",
	}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if opts.version != "v9.9.9" || opts.commit != "abc123" || opts.date != "2026-01-02T03:04:05Z" {
		t.Errorf("metadata not honored: %+v", opts)
	}
	if opts.outDir != "out" || len(opts.targets) != 1 || opts.targets[0].String() != "windows/amd64" {
		t.Errorf("targets/output not honored: %+v", opts)
	}
	if !opts.checksums {
		t.Error("checksums should default true")
	}
}

func TestParseFlagsBadTarget(t *testing.T) {
	if _, err := parseFlags([]string{"-targets", "not-a-target"}, &bytes.Buffer{}); err == nil {
		t.Error("parseFlags with a bad target should error")
	}
}

// TestRunEndToEnd exercises the whole pipeline — cross-compile, package, and
// checksum — against a small in-module package (this release tool itself), so it
// stays fast and independent of the daemon's cross-compile state.
func TestRunEndToEnd(t *testing.T) {
	orig := buildPackage
	buildPackage = "./"
	defer func() { buildPackage = orig }()

	out := t.TempDir()
	var stdout, stderr bytes.Buffer
	err := run([]string{
		"-version", "v1.2.3", "-commit", "deadbee", "-date", "2026-01-02T03:04:05Z",
		"-targets", "windows/amd64", "-output", out,
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run: %v\nstderr: %s", err, stderr.String())
	}

	// Archive present and correctly named.
	archive := filepath.Join(out, "goobers_v1.2.3_windows_amd64.zip")
	if _, err := os.Stat(archive); err != nil {
		t.Fatalf("expected archive %s: %v", archive, err)
	}
	// Zip contains goobers.exe (the built binary renamed to the target's name).
	zr, err := zip.OpenReader(archive)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = zr.Close() }()
	if len(zr.File) != 1 || zr.File[0].Name != "goobers.exe" {
		t.Fatalf("zip contents = %v, want [goobers.exe]", zr.File)
	}
	// SHA256SUMS written and references the archive.
	sums, err := os.ReadFile(filepath.Join(out, "SHA256SUMS"))
	if err != nil {
		t.Fatalf("SHA256SUMS: %v", err)
	}
	if !strings.Contains(string(sums), "goobers_v1.2.3_windows_amd64.zip") {
		t.Errorf("SHA256SUMS missing the archive:\n%s", sums)
	}
	// The intermediate binary was cleaned up, leaving only the archive + sums.
	entries, _ := os.ReadDir(out)
	if len(entries) != 2 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("dist has %v, want exactly the archive + SHA256SUMS", names)
	}
}

// TestRunSkipUnbuildable proves the skip path: an impossible target is skipped
// (not fatal) under -skip-unbuildable, and produces no partial artifacts.
func TestRunSkipUnbuildable(t *testing.T) {
	orig := buildPackage
	buildPackage = "./"
	defer func() { buildPackage = orig }()

	out := t.TempDir()
	var stdout, stderr bytes.Buffer
	err := run([]string{
		"-targets", "windows/ppc64", // an unsupported GOOS/GOARCH pair: fails fast
		"-skip-unbuildable", "-output", out,
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run with -skip-unbuildable should not fail: %v", err)
	}
	if !strings.Contains(stdout.String(), "skip") {
		t.Errorf("expected a skip notice, got:\n%s", stdout.String())
	}
	entries, _ := os.ReadDir(out)
	if len(entries) != 0 {
		t.Errorf("no artifacts should be produced when the only target is skipped, got %d", len(entries))
	}
}
