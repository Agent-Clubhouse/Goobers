// Command release cross-compiles the goobers binary for the release target
// matrix, packages each into a platform-conventional archive, and emits a
// shared SHA256SUMS manifest — the packaging primitive a tagged-release
// workflow (REL-2, #432) invokes. It is deliberately a standalone `go run`
// tool (matching test/ci and test/coveragegate), not a Makefile shell block,
// so it is portable across the release runners this milestone must support
// (#655, milestone #12/#17).
//
// Windows (#655) is packaged as a .zip; unix targets as .tar.gz, both named
// goobers_<version>_<os>_<arch>.<ext>. Each archive carries the target binary
// plus the release-pinned onboarding docs. Build metadata (version/commit/date)
// is injected via the same -ldflags path the Makefile uses (internal/version),
// so release binaries report identical `goobers --version` output to a local
// `make build`.
package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
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

type archiveEntry struct {
	name string
	mode int64
	data []byte
}

// packageArchive writes one release archive at outDir/<archiveName>. The
// optional release root is included beside the target binary; callers that omit
// it retain the binary-only primitive used by focused packaging tests.
func packageArchive(t Target, version, binPath, outDir string, releaseRoot ...string) (string, error) {
	if len(releaseRoot) > 1 {
		return "", fmt.Errorf("package archive accepts at most one release root")
	}
	archivePath := filepath.Join(outDir, t.archiveName(version))
	data, err := os.ReadFile(binPath)
	if err != nil {
		return "", fmt.Errorf("read built binary %s: %w", binPath, err)
	}
	entries := []archiveEntry{{name: t.binaryName(), mode: 0o755, data: data}}
	if len(releaseRoot) == 1 {
		releaseEntries, err := collectArchiveEntries(releaseRoot[0])
		if err != nil {
			return "", err
		}
		entries = append(entries, releaseEntries...)
	}
	f, err := os.Create(archivePath)
	if err != nil {
		return "", fmt.Errorf("create archive %s: %w", archivePath, err)
	}
	defer func() { _ = f.Close() }()

	if t.archiveExt() == "zip" {
		if err := writeZip(f, entries); err != nil {
			return "", fmt.Errorf("write zip %s: %w", archivePath, err)
		}
	} else if err := writeTarGz(f, entries); err != nil {
		return "", fmt.Errorf("write tar.gz %s: %w", archivePath, err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("close archive %s: %w", archivePath, err)
	}
	return archivePath, nil
}

func collectArchiveEntries(root string) ([]archiveEntry, error) {
	var entries []archiveEntry
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("release archive root must not contain symlink %s", path)
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("inspect release archive entry %s: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("release archive root contains unsupported file %s", path)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return fmt.Errorf("resolve release archive entry %s: %w", path, err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read release archive entry %s: %w", path, err)
		}
		entries = append(entries, archiveEntry{
			name: filepath.ToSlash(rel),
			mode: 0o644,
			data: data,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
	return entries, nil
}

func writeZip(w io.Writer, entries []archiveEntry) error {
	zw := zip.NewWriter(w)
	for _, entry := range entries {
		hdr := &zip.FileHeader{Name: entry.name, Method: zip.Deflate}
		hdr.SetMode(fs.FileMode(entry.mode))
		fw, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		if _, err := fw.Write(entry.data); err != nil {
			return err
		}
	}
	return zw.Close()
}

func writeTarGz(w io.Writer, entries []archiveEntry) error {
	gw := gzip.NewWriter(w)
	tw := tar.NewWriter(gw)
	for _, entry := range entries {
		if err := tw.WriteHeader(&tar.Header{
			Name: entry.name,
			Mode: entry.mode,
			Size: int64(len(entry.data)),
		}); err != nil {
			return err
		}
		if _, err := tw.Write(entry.data); err != nil {
			return err
		}
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
