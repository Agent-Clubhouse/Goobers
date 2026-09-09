package configmirror

import (
	"archive/zip"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Snapshot pins one opened archive for the entire seed operation, even if a
// publisher replaces the public path meanwhile. Workers need only read access.
type Snapshot struct {
	file    *os.File
	archive *zip.Reader
}

// Open reads a bounded archive without acquiring the daemon's publication lock.
func Open(directory string) (*Snapshot, error) {
	return openSnapshot(filepath.Join(directory, SnapshotName))
}

func openSnapshot(path string) (*Snapshot, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err == nil && (!info.Mode().IsRegular() || info.Size() > MaxSnapshotBytes+(64<<20)) {
		err = errors.New("invalid config mirror archive size or type")
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := admitArchive(f, info.Size()); err != nil {
		_ = f.Close()
		return nil, err
	}
	archive, err := zip.NewReader(f, info.Size())
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if len(archive.File) > MaxFiles {
		_ = f.Close()
		return nil, errors.New("config mirror contains too many files")
	}
	return &Snapshot{file: f, archive: archive}, nil
}

// Close releases the pinned archive handle.
func (s *Snapshot) Close() error { return s.file.Close() }

// Extract writes only new files beneath destination. Callers must extract into
// a private staging directory and publish it only after this method succeeds.
// Links and special mode bits are rejected; ordinary source permissions survive.
func (s *Snapshot) Extract(ctx context.Context, destination string) error {
	root, err := os.OpenRoot(destination)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	var total int64
	seen := make(map[string]bool, len(s.archive.File))
	var directories []*zip.File
	for _, entry := range s.archive.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !validEntry(entry) {
			return errors.New("invalid config mirror entry")
		}
		if err := reserveSnapshotName(seen, strings.TrimSuffix(entry.Name, "/")); err != nil {
			return err
		}
		if entry.Mode().IsDir() {
			if err := root.MkdirAll(strings.TrimSuffix(entry.Name, "/"), 0o700); err != nil {
				return err
			}
			directories = append(directories, entry)
			continue
		}
		n, err := extractEntry(root, entry)
		total += n
		if err != nil {
			return err
		}
		if total > MaxSnapshotBytes {
			return errors.New("config mirror exceeds total extraction limit")
		}
	}
	if !seen["instance.yaml"] {
		return errors.New("config mirror has no instance document")
	}
	// Apply deepest directories first, after writing all descendants. Source
	// directories may be read-only, and archive order is not authoritative.
	sort.Slice(directories, func(i, j int) bool { return len(directories[i].Name) > len(directories[j].Name) })
	for _, entry := range directories {
		if err := root.Chmod(strings.TrimSuffix(entry.Name, "/"), entry.Mode().Perm()); err != nil {
			return err
		}
	}
	return nil
}

func validEntry(entry *zip.File) bool {
	if !validSnapshotMode(entry.Mode()) || entry.UncompressedSize64 > MaxFileBytes {
		return false
	}
	if entry.Mode().IsDir() {
		return strings.HasSuffix(entry.Name, "/") && entry.UncompressedSize64 == 0 && strings.HasPrefix(entry.Name, "config/") && validSnapshotName(strings.TrimSuffix(entry.Name, "/"))
	}
	return entry.Name != "config" && validSnapshotName(entry.Name)
}

func reserveSnapshotName(seen map[string]bool, name string) error {
	key := strings.ToLower(name)
	if !validSnapshotName(name) || seen[key] {
		return errors.New("invalid or duplicate config mirror path")
	}
	for prefix := name; prefix != "."; prefix = path.Dir(prefix) {
		folded, exact := "fold:"+strings.ToLower(prefix), "exact:"+prefix
		if seen[folded] && !seen[exact] {
			return errors.New("config mirror path components differ only by case")
		}
	}
	for prefix := name; prefix != "."; prefix = path.Dir(prefix) {
		seen["fold:"+strings.ToLower(prefix)], seen["exact:"+prefix] = true, true
	}
	seen[key] = true
	return nil
}

func validSnapshotName(name string) bool {
	if !fs.ValidPath(name) || len(name) > 4096 || strings.ContainsAny(name, "\\:*?<>|\"") || strings.IndexFunc(name, func(r rune) bool { return r < 32 }) >= 0 {
		return false
	}
	if name != "instance.yaml" && name != "config" && !strings.HasPrefix(name, "config/") {
		return false
	}
	for _, segment := range strings.Split(name, "/") {
		if strings.HasSuffix(segment, ".") || strings.HasSuffix(segment, " ") {
			return false
		}
		base := strings.ToUpper(strings.SplitN(segment, ".", 2)[0])
		switch base {
		case "CON", "PRN", "AUX", "NUL":
			return false
		}
		if len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9' {
			return false
		}
	}
	return true
}

func extractEntry(root *os.Root, entry *zip.File) (int64, error) {
	if err := root.MkdirAll(path.Dir(entry.Name), 0o750); err != nil {
		return 0, err
	}
	reader, err := entry.Open()
	if err != nil {
		return 0, err
	}
	defer func() { _ = reader.Close() }()
	f, err := root.OpenFile(entry.Name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return 0, err
	}
	n, copyErr := io.Copy(f, io.LimitReader(reader, MaxFileBytes+1))
	modeErr := f.Chmod(entry.Mode().Perm())
	closeErr := f.Close()
	if n > MaxFileBytes {
		return n, errors.New("config mirror file exceeds extraction limit")
	}
	return n, errors.Join(copyErr, modeErr, closeErr)
}
