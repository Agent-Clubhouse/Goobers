// Command release cross-compiles the goobers binary for the release target
// matrix, packages each into a platform-conventional archive, and emits a
// shared SHA256SUMS manifest — the packaging primitive a tagged-release
// workflow (REL-2, #432) invokes. It is deliberately a standalone `go run`
// tool (matching test/ci and test/coveragegate), not a Makefile shell block,
// so it is portable across the release runners this milestone must support
// (#655, milestone #12/#17).
//
// Windows (#655) is packaged as a .zip of goobers.exe; unix targets as
// .tar.gz of goobers, both named goobers_<version>_<os>_<arch>.<ext>. Build
// metadata (version/commit/date) is injected via the same -ldflags path the
// Makefile uses (internal/version), so release binaries report identical
// `goobers --version` output to a local `make build`.
package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Target is one row of the release build matrix.
type Target struct {
	OS   string // GOOS
	Arch string // GOARCH
}

// String renders the target as "os/arch" (e.g. "windows/amd64").
func (t Target) String() string { return t.OS + "/" + t.Arch }

// binaryName is the goobers executable name for this target — .exe on Windows,
// bare elsewhere, matching what `go build` itself produces.
func (t Target) binaryName() string {
	if t.OS == "windows" {
		return "goobers.exe"
	}
	return "goobers"
}

// archiveExt is the archive extension for this target: .zip on Windows (the
// platform convention Windows users expect, and what scoop/winget consume),
// .tar.gz elsewhere.
func (t Target) archiveExt() string {
	if t.OS == "windows" {
		return "zip"
	}
	return "tar.gz"
}

// archiveName is the release artifact filename, e.g.
// "goobers_v1.2.3_windows_amd64.zip". Version keeps its leading "v" if present
// (matching git tag form); a "dev"/untagged version still yields a valid name.
func (t Target) archiveName(version string) string {
	return fmt.Sprintf("goobers_%s_%s_%s.%s", version, t.OS, t.Arch, t.archiveExt())
}

// DefaultTargets is the release matrix. windows/amd64 is this issue's (#655)
// addition to the existing darwin/linux set. windows/arm64 is intentionally
// absent: Go cross-compiles it cheaply, but nothing in CI or on a real machine
// has executed it, and shipping a never-run binary is exactly the false-green
// trap the release gate exists to avoid (#655 AC / P12) — it is promoted here
// only once something runs it.
var DefaultTargets = []Target{
	{OS: "darwin", Arch: "amd64"},
	{OS: "darwin", Arch: "arm64"},
	{OS: "linux", Arch: "amd64"},
	{OS: "linux", Arch: "arm64"},
	{OS: "windows", Arch: "amd64"},
}

// packageArchive writes one release archive at outDir/<archiveName> containing
// the single binary at binPath under its target-appropriate name, and returns
// the archive path. Zip on Windows, gzip'd tar elsewhere.
func packageArchive(t Target, version, binPath, outDir string) (string, error) {
	archivePath := filepath.Join(outDir, t.archiveName(version))
	data, err := os.ReadFile(binPath)
	if err != nil {
		return "", fmt.Errorf("read built binary %s: %w", binPath, err)
	}
	f, err := os.Create(archivePath)
	if err != nil {
		return "", fmt.Errorf("create archive %s: %w", archivePath, err)
	}
	defer func() { _ = f.Close() }()

	if t.archiveExt() == "zip" {
		if err := writeZip(f, t.binaryName(), data); err != nil {
			return "", fmt.Errorf("write zip %s: %w", archivePath, err)
		}
	} else if err := writeTarGz(f, t.binaryName(), data); err != nil {
		return "", fmt.Errorf("write tar.gz %s: %w", archivePath, err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("close archive %s: %w", archivePath, err)
	}
	return archivePath, nil
}

// writeZip writes a single-entry zip (name -> data) with the executable bit set
// — harmless on Windows, and correct if the zip is ever unpacked on a unix box.
func writeZip(w io.Writer, name string, data []byte) error {
	zw := zip.NewWriter(w)
	hdr := &zip.FileHeader{Name: name, Method: zip.Deflate}
	hdr.SetMode(0o755)
	fw, err := zw.CreateHeader(hdr)
	if err != nil {
		return err
	}
	if _, err := fw.Write(data); err != nil {
		return err
	}
	return zw.Close()
}

// writeTarGz writes a single-entry gzip'd tar (name -> data) with the
// executable bit set.
func writeTarGz(w io.Writer, name string, data []byte) error {
	gw := gzip.NewWriter(w)
	tw := tar.NewWriter(gw)
	if err := tw.WriteHeader(&tar.Header{
		Name: name,
		Mode: 0o755,
		Size: int64(len(data)),
	}); err != nil {
		return err
	}
	if _, err := tw.Write(data); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gw.Close()
}

// sha256Hex returns the lowercase hex SHA-256 of the file at path.
func sha256Hex(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// checksumsManifest renders a coreutils `sha256sum -c`-compatible manifest for
// the given archive paths — "<hex>  <basename>\n" per line, sorted by filename
// for reproducibility. PowerShell's Get-FileHash produces the same hex (see the
// Windows install docs), so the one manifest verifies on every platform.
func checksumsManifest(archivePaths []string) (string, error) {
	lines := make([]string, 0, len(archivePaths))
	for _, p := range archivePaths {
		sum, err := sha256Hex(p)
		if err != nil {
			return "", fmt.Errorf("checksum %s: %w", p, err)
		}
		lines = append(lines, fmt.Sprintf("%s  %s", sum, filepath.Base(p)))
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n") + "\n", nil
}
