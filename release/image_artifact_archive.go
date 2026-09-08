package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
)

const (
	imageArtifactManifestLimit int64 = 1 << 20
	imageArtifactArchiveLimit  int64 = 512 << 20
	imageArtifactExpandedLimit int64 = 1 << 30
	imageArtifactBinaryLimit   int64 = 128 << 20
	imageArtifactEntryLimit          = 10000
)

// readImageArtifactManifest accepts the exact sha256sum text form emitted by
// release packaging. A checksum proves byte identity, not publisher identity;
// callers remain responsible for obtaining trusted release artifacts.
func readImageArtifactManifest(directory string) (map[string]string, error) {
	data, err := readRegularImageArtifact(directory, "SHA256SUMS", imageArtifactManifestLimit)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) > imageArtifactEntryLimit {
		return nil, fmt.Errorf("SHA256SUMS exceeds entry limit")
	}
	manifest := make(map[string]string, len(lines))
	seen := make(map[string]bool, len(lines))
	for _, line := range lines {
		if len(line) < 67 || line[64:66] != "  " {
			return nil, fmt.Errorf("malformed SHA256SUMS entry")
		}
		digest, name := line[:64], line[66:]
		decoded, err := hex.DecodeString(digest)
		if err != nil || len(decoded) != sha256.Size || !safeImageArtifactPath(name) || strings.EqualFold(name, "SHA256SUMS") {
			return nil, fmt.Errorf("invalid SHA256SUMS digest or path")
		}
		key := strings.ToLower(name)
		if seen[key] {
			return nil, fmt.Errorf("duplicate SHA256SUMS path %q", name)
		}
		seen[key] = true
		manifest[name] = strings.ToLower(digest)
	}
	return manifest, nil
}

func safeImageArtifactPath(name string) bool {
	if len(name) > 1024 || name == "." || !fs.ValidPath(name) || strings.ContainsAny(name, "\\:") {
		return false
	}
	for _, char := range name {
		if char <= ' ' || char == 127 {
			return false
		}
	}
	for _, part := range strings.Split(name, "/") {
		if strings.HasSuffix(part, ".") || imageArtifactDeviceName(part) {
			return false
		}
	}
	return true
}

// Refuse DOS device aliases even when inspection is running on Unix. Windows
// resolves these independently of ordinary filesystem names and extensions.
func imageArtifactDeviceName(component string) bool {
	base, _, _ := strings.Cut(strings.ToUpper(component), ".")
	switch base {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return true
	}
	return len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9'
}

// Inspect every component below the caller's trusted root. os.Root also keeps
// opens confined if an input is replaced concurrently; comparing file identity
// catches replacements between inspection and opening. Parent symlinks outside
// this root (notably macOS /tmp) do not make an otherwise valid input unsafe.
func readRegularImageArtifact(directory, name string, maxBytes int64) ([]byte, error) {
	if !safeImageArtifactPath(name) || maxBytes <= 0 || maxBytes > imageArtifactExpandedLimit {
		return nil, fmt.Errorf("invalid artifact path or size limit")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, fmt.Errorf("open artifact directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	components := strings.Split(name, "/")
	for index := 1; index < len(components); index++ {
		info, err := root.Lstat(strings.Join(components[:index], "/"))
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("artifact parent must be a nonsymlink directory")
		}
	}
	info, err := root.Lstat(name)
	if err != nil {
		return nil, fmt.Errorf("inspect artifact %s: %w", name, err)
	}
	if !info.Mode().IsRegular() || info.Size() > maxBytes {
		return nil, fmt.Errorf("artifact %s must be a bounded regular nonsymlink file", name)
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(info, opened) || opened.Size() != info.Size() {
		return nil, fmt.Errorf("artifact %s changed while opening", name)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes || int64(len(data)) != info.Size() {
		return nil, fmt.Errorf("artifact %s exceeds size limit or changed while reading", name)
	}
	return data, nil
}

func readVerifiedImageArtifact(directory, name string, manifest map[string]string, maxBytes int64) ([]byte, error) {
	expected, ok := manifest[name]
	if !ok {
		return nil, fmt.Errorf("SHA256SUMS lacks exact artifact %q", name)
	}
	data, err := readRegularImageArtifact(directory, name, maxBytes)
	if err != nil {
		return nil, err
	}
	actual := fmt.Sprintf("%x", sha256.Sum256(data))
	if actual != expected {
		return nil, fmt.Errorf("artifact %s SHA256 mismatch", name)
	}
	return data, nil
}

type finalArchiveBinary struct {
	Data          []byte
	ArchiveName   string
	ArchiveSHA256 string
}

// readFinalArchiveBinary imports the final release bytes, including any signing
// overlay. It performs no extraction to disk, code execution or network access.
func readFinalArchiveBinary(assetsDir, version string, target Target) (finalArchiveBinary, error) {
	name := target.archiveName(version)
	if !safeImageArtifactPath(name) || strings.Contains(name, "/") {
		return finalArchiveBinary{}, fmt.Errorf("invalid final release archive name")
	}
	manifest, err := readImageArtifactManifest(assetsDir)
	if err != nil {
		return finalArchiveBinary{}, err
	}
	data, err := readVerifiedImageArtifact(assetsDir, name, manifest, imageArtifactArchiveLimit)
	if err != nil {
		return finalArchiveBinary{}, err
	}
	var binary []byte
	if target.OS == "windows" {
		binary, err = readFinalZIPBinary(data, target.binaryName())
	} else {
		binary, err = readFinalTarBinary(data, target.binaryName())
	}
	if err != nil {
		return finalArchiveBinary{}, err
	}
	return finalArchiveBinary{Data: binary, ArchiveName: name, ArchiveSHA256: manifest[name]}, nil
}

type imageArchiveEntries struct {
	names    map[string]bool
	parents  map[string]bool
	count    int
	expanded int64
	binary   []byte
}

func (entries *imageArchiveEntries) accept(name string, mode fs.FileMode, size int64) error {
	directory := mode.IsDir()
	if directory {
		name = strings.TrimSuffix(name, "/")
	}
	if !safeImageArtifactPath(name) || (!directory && !mode.IsRegular()) || mode&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky) != 0 {
		return fmt.Errorf("unsafe archive entry %q", name)
	}
	if entries.names == nil {
		entries.names = make(map[string]bool)
		entries.parents = make(map[string]bool)
	}
	key := strings.ToLower(name)
	if _, found := entries.names[key]; found || (!directory && entries.parents[key]) {
		return fmt.Errorf("duplicate or conflicting archive entry %q", name)
	}
	for parent := path.Dir(key); parent != "."; parent = path.Dir(parent) {
		if isDir, found := entries.names[parent]; found && !isDir {
			return fmt.Errorf("archive path traverses a file")
		}
		entries.parents[parent] = true
	}
	entries.names[key] = directory
	entries.count++
	if entries.count > imageArtifactEntryLimit || size < 0 || size > imageArtifactExpandedLimit-entries.expanded || (directory && size != 0) {
		return fmt.Errorf("archive exceeds entry or expanded size limits")
	}
	entries.expanded += size
	return nil
}

func (entries *imageArchiveEntries) readBody(reader io.Reader, name, binaryName string, size int64, executable bool) error {
	if name == binaryName {
		if !executable || size <= 0 || size > imageArtifactBinaryLimit {
			return fmt.Errorf("archive binary must be a bounded regular executable")
		}
		data, err := io.ReadAll(io.LimitReader(reader, size+1))
		if err != nil {
			return err
		}
		if int64(len(data)) != size {
			return fmt.Errorf("archive binary size mismatch")
		}
		entries.binary = data
		return nil
	}
	n, err := io.Copy(io.Discard, io.LimitReader(reader, size+1))
	if err != nil {
		return err
	}
	if n != size {
		return fmt.Errorf("archive entry size mismatch")
	}
	return nil
}

func (entries *imageArchiveEntries) result() ([]byte, error) {
	if entries.binary == nil {
		return nil, fmt.Errorf("archive lacks exactly one top-level target binary")
	}
	return entries.binary, nil
}

func readFinalZIPBinary(data []byte, binaryName string) ([]byte, error) {
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("read final ZIP archive: %w", err)
	}
	end := len(data) - 22 - len(reader.Comment)
	if len(reader.File) > imageArtifactEntryLimit || end < 0 || !bytes.HasPrefix(data, []byte("PK\x03\x04")) || !bytes.Equal(data[end:end+4], []byte("PK\x05\x06")) {
		return nil, fmt.Errorf("invalid ZIP envelope or entry limit")
	}
	var entries imageArchiveEntries
	for _, file := range reader.File {
		if file.UncompressedSize64 > uint64(imageArtifactExpandedLimit) || file.Flags&1 != 0 {
			return nil, fmt.Errorf("ZIP entry exceeds size limit or is encrypted")
		}
		size := int64(file.UncompressedSize64)
		if err := entries.accept(file.Name, file.Mode(), size); err != nil {
			return nil, err
		}
		body, err := file.Open()
		if err != nil {
			return nil, err
		}
		// Windows-created ZIPs have DOS attributes, not POSIX execute bits. Unix
		// and macOS producers retain those bits and must mark the binary executable.
		creator := file.CreatorVersion >> 8
		executable := file.Mode().IsRegular() && (file.Mode().Perm()&0o111 != 0 || (creator != 3 && creator != 19))
		readErr := entries.readBody(body, file.Name, binaryName, size, executable)
		if err := errors.Join(readErr, body.Close()); err != nil {
			return nil, err
		}
	}
	return entries.result()
}

func readFinalTarBinary(data []byte, binaryName string) ([]byte, error) {
	source := bytes.NewReader(data)
	gz, err := gzip.NewReader(source)
	if err != nil {
		return nil, fmt.Errorf("read final gzip archive: %w", err)
	}
	defer func() { _ = gz.Close() }()
	gz.Multistream(false)
	expanded := &io.LimitedReader{R: gz, N: imageArtifactExpandedLimit + 1}
	reader := tar.NewReader(expanded)
	var entries imageArchiveEntries
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read final tar archive: %w", err)
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeDir {
			return nil, fmt.Errorf("archive contains a link, device or unsupported entry")
		}
		if err := entries.accept(header.Name, header.FileInfo().Mode(), header.Size); err != nil {
			return nil, err
		}
		if err := entries.readBody(reader, header.Name, binaryName, header.Size, header.Typeflag == tar.TypeReg && header.Mode&0o111 != 0); err != nil {
			return nil, err
		}
	}
	// Consume the gzip trailer (including CRC) and reject hidden appended tar
	// content, concatenated gzip members, and decompression bombs in padding.
	if _, err := io.Copy(imageArchiveZeroPadding{}, expanded); err != nil {
		return nil, fmt.Errorf("invalid tar archive trailer: %w", err)
	}
	if expanded.N == 0 || source.Len() != 0 {
		return nil, fmt.Errorf("archive exceeds expanded limit or has trailing gzip data")
	}
	return entries.result()
}

type imageArchiveZeroPadding struct{}

func (imageArchiveZeroPadding) Write(data []byte) (int, error) {
	for _, value := range data {
		if value != 0 {
			return 0, fmt.Errorf("nonzero data after tar end")
		}
	}
	return len(data), nil
}
