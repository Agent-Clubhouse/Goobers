// Package gooberassets loads a goober's optional static asset bundle and
// materializes an isolated snapshot for each harness invocation.
package gooberassets

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"golang.org/x/sys/unix"
)

const (
	// SourceDir is the fixed directory beside goober.yaml that contains assets.
	SourceDir = "assets"
	// WorkspaceDir is the fixed workspace-relative path where assets appear.
	WorkspaceDir = ".goober-assets"
)

// ErrWorkspaceCollision means the target repository already owns WorkspaceDir.
var ErrWorkspaceCollision = errors.New("goober assets workspace path already exists")

type entry struct {
	path string
	mode fs.FileMode
	data []byte
	dir  bool
}

// Bundle is an immutable in-memory snapshot of one goober's assets.
type Bundle struct {
	rootMode fs.FileMode
	entries  []entry
}

// IsSourceDir reports whether path has the fixed goober assets layout:
// .../goobers/<name>/assets.
func IsSourceDir(path string) bool {
	clean := filepath.Clean(path)
	if filepath.Base(clean) != SourceDir {
		return false
	}
	return filepath.Base(filepath.Dir(filepath.Dir(clean))) == "goobers"
}

// IsWithinSourceDir reports whether path is a goober assets directory or one
// of its descendants.
func IsWithinSourceDir(path string) bool {
	for current := filepath.Clean(path); ; {
		if IsSourceDir(current) {
			return true
		}
		parent := filepath.Dir(current)
		if parent == current {
			return false
		}
		current = parent
	}
}

// Validate checks the bundle's structure without reading file contents.
func Validate(source string) error {
	_, err := scan(source, false)
	return err
}

// Load snapshots source. A missing source is the supported no-assets case.
func Load(source string) (*Bundle, error) {
	return scan(source, true)
}

func scan(source string, readContents bool) (*Bundle, error) {
	root, err := openAsset(source)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect asset directory: %w", err)
	}
	defer func() { _ = root.Close() }()

	info, err := root.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect asset directory: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("asset path %q must be a directory", source)
	}

	bundle := &Bundle{rootMode: info.Mode()}
	if err := scanDirectory(root, source, "", bundle, readContents); err != nil {
		return nil, fmt.Errorf("load assets from %q: %w", source, err)
	}
	return bundle, nil
}

func scanDirectory(dir *os.File, source, relative string, bundle *Bundle, readContents bool) error {
	children, err := dir.ReadDir(-1)
	if err != nil {
		return err
	}
	sort.Slice(children, func(i, j int) bool { return children[i].Name() < children[j].Name() })
	for _, child := range children {
		rel := filepath.Join(relative, child.Name())
		if err := scanEntry(dir, source, rel, bundle, readContents); err != nil {
			return err
		}
	}
	return nil
}

func scanEntry(parent *os.File, source, relative string, bundle *Bundle, readContents bool) error {
	file, err := openAssetAt(parent, filepath.Base(relative))
	if err != nil {
		return fmt.Errorf("inspect asset %q: %w", filepath.Join(source, relative), err)
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("inspect asset %q: %w", filepath.Join(source, relative), err)
	}
	switch {
	case info.IsDir():
		bundle.entries = append(bundle.entries, entry{path: relative, mode: info.Mode(), dir: true})
		return scanDirectory(file, source, relative, bundle, readContents)
	case info.Mode().IsRegular():
		var data []byte
		if readContents {
			data, err = io.ReadAll(file)
			if err != nil {
				return fmt.Errorf("read asset %q: %w", filepath.Join(source, relative), err)
			}
		}
		bundle.entries = append(bundle.entries, entry{path: relative, mode: info.Mode(), data: data})
		return nil
	default:
		return fmt.Errorf("asset %q must be a regular file or directory", filepath.Join(source, relative))
	}
}

const assetOpenFlags = unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK

func openAsset(path string) (*os.File, error) {
	fd, err := unix.Open(path, assetOpenFlags, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, fmt.Errorf("asset %q must not be a symlink: %w", path, err)
		}
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func openAssetAt(parent *os.File, name string) (*os.File, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, assetOpenFlags, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, fmt.Errorf("must not be a symlink: %w", err)
		}
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

// Fingerprint returns a stable digest of the bundle's paths, modes, and bytes.
func (b *Bundle) Fingerprint() string {
	hash := sha256.New()
	writeFingerprintEntry(hash, ".", b.rootMode, nil)
	for _, asset := range b.entries {
		writeFingerprintEntry(hash, filepath.ToSlash(asset.path), asset.mode, asset.data)
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

func writeFingerprintEntry(w io.Writer, path string, mode fs.FileMode, content []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(path)))
	_, _ = w.Write(size[:])
	_, _ = w.Write([]byte(path))
	binary.BigEndian.PutUint64(size[:], uint64(mode))
	_, _ = w.Write(size[:])
	binary.BigEndian.PutUint64(size[:], uint64(len(content)))
	_, _ = w.Write(size[:])
	_, _ = w.Write(content)
}

// EnsureWorkspaceAvailable rejects any pre-existing reserved asset path.
func EnsureWorkspaceAvailable(workspace string) error {
	if workspace == "" {
		return errors.New("goober assets workspace is empty")
	}
	target := filepath.Join(workspace, WorkspaceDir)
	if _, err := os.Lstat(target); err == nil {
		return fmt.Errorf("%w: %s", ErrWorkspaceCollision, WorkspaceDir)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("inspect asset workspace path: %w", err)
	}
	return nil
}

// Materialize writes the bundle beneath workspace/WorkspaceDir. A nil bundle
// is the supported no-assets case and leaves the workspace unchanged. Existing
// content at that path is a collision and is never replaced by a real bundle.
func (b *Bundle) Materialize(workspace string) (err error) {
	if b == nil {
		return nil
	}
	if err := EnsureWorkspaceAvailable(workspace); err != nil {
		return err
	}
	target := filepath.Join(workspace, WorkspaceDir)
	if err := os.Mkdir(target, 0o700); err != nil {
		return fmt.Errorf("create asset workspace: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(target)
		}
	}()

	var dirs []entry
	for _, asset := range b.entries {
		path := filepath.Join(target, asset.path)
		if asset.dir {
			if err := os.Mkdir(path, 0o700); err != nil {
				return fmt.Errorf("create asset directory %q: %w", asset.path, err)
			}
			dirs = append(dirs, asset)
			continue
		}
		if err := os.WriteFile(path, asset.data, 0o600); err != nil {
			return fmt.Errorf("write asset %q: %w", asset.path, err)
		}
		if err := os.Chmod(path, asset.mode.Perm()); err != nil {
			return fmt.Errorf("set asset mode %q: %w", asset.path, err)
		}
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := os.Chmod(filepath.Join(target, dirs[i].path), dirs[i].mode.Perm()); err != nil {
			return fmt.Errorf("set asset directory mode %q: %w", dirs[i].path, err)
		}
	}
	if err := os.Chmod(target, b.rootMode.Perm()); err != nil {
		return fmt.Errorf("set asset root mode: %w", err)
	}
	return nil
}
