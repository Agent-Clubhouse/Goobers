// Package configmirror transports a rendered worker configuration as one
// atomically replaced archive. Readers hold one opened snapshot, never a mix
// of files from successive daemon reloads.
package configmirror

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/goobers/goobers/internal/platform/durability"
	"github.com/goobers/goobers/internal/platform/lock"
)

// SnapshotName is the single public artifact. Limits bound publication and
// extraction independently of the much smaller Kubernetes ConfigMap ceiling.
const (
	SnapshotName     = "worker-config.zip"
	MaxFiles         = 10000
	MaxFileBytes     = 64 << 20
	MaxSnapshotBytes = 1 << 30
)

// Publish copies only the supplied instance document and rendered config tree.
// The caller must supply worker-safe instance bytes after validating the tree.
// A fixed staging filename, protected by a writer lock, also reclaims a prior
// crashed publication without accumulating unbounded temporary directories.
func Publish(ctx context.Context, destination, configDir string, instanceDocument []byte) error {
	if !filepath.IsAbs(destination) || !filepath.IsAbs(configDir) {
		return errors.New("config mirror requires absolute paths")
	}
	if len(instanceDocument) == 0 || len(instanceDocument) > MaxFileBytes {
		return errors.New("invalid mirrored instance document size")
	}
	rel, err := filepath.Rel(configDir, destination)
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("config mirror cannot be inside its source tree")
	}
	if err := os.MkdirAll(destination, 0o750); err != nil {
		return err
	}
	held, err := lock.TryAcquire(filepath.Join(destination, "publish.lock"))
	if err != nil {
		return err
	}
	defer func() { _ = held.Release() }()
	staged := filepath.Join(destination, ".worker-config.pending")
	if err := removeStagingFile(staged); err != nil {
		return err
	}
	f, err := os.OpenFile(staged, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close(); _ = os.Remove(staged) }()
	if err := writeSnapshot(ctx, f, configDir, instanceDocument); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return durability.ReplaceFile(staged, filepath.Join(destination, SnapshotName))
}

func removeStagingFile(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("config mirror staging path is not a regular file")
	}
	return os.Remove(path)
}

func writeSnapshot(ctx context.Context, out io.Writer, configDir string, document []byte) error {
	root, err := os.OpenRoot(configDir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	archive := zip.NewWriter(out)
	if err := writeEntry(archive, "instance.yaml", strings.NewReader(string(document))); err != nil {
		return err
	}
	count, total := 1, int64(len(document))
	err = fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if !validSnapshotName("config/" + path) {
			return fmt.Errorf("config mirror refuses non-portable path %q", path)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("config mirror refuses non-regular file %q", path)
		}
		count++
		if count > MaxFiles || info.Size() > MaxFileBytes || total+info.Size() > MaxSnapshotBytes {
			return errors.New("config mirror snapshot exceeds safety limits")
		}
		file, err := root.Open(path)
		if err != nil {
			return err
		}
		defer func() { _ = file.Close() }()
		// Check the opened object too: a tree changing during publication must
		// not bypass the size bound using stale WalkDir metadata.
		opened, err := file.Stat()
		if err != nil {
			return err
		}
		if !opened.Mode().IsRegular() {
			return fmt.Errorf("config mirror opened a non-regular file %q", path)
		}
		writer, err := archive.CreateHeader(&zip.FileHeader{Name: "config/" + path, Method: zip.Store})
		if err != nil {
			return err
		}
		n, err := io.Copy(writer, io.LimitReader(file, MaxFileBytes+1))
		total += n
		if n > MaxFileBytes || total > MaxSnapshotBytes {
			return errors.New("config mirror snapshot exceeds safety limits")
		}
		return err
	})
	if err != nil {
		return err
	}
	return archive.Close()
}

func writeEntry(archive *zip.Writer, name string, content io.Reader) error {
	w, err := archive.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
	if err != nil {
		return err
	}
	_, err = io.Copy(w, content)
	return err
}
