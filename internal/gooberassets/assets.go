// Package gooberassets loads a goober's optional static asset bundle and
// materializes an isolated snapshot for each harness invocation.
package gooberassets

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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

// Load snapshots source. A missing source is the supported no-assets case.
// Symlinks and non-regular files are rejected so a bundle cannot expose
// content outside its definition directory.
func Load(source string) (*Bundle, error) {
	root, err := os.Lstat(source)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect asset directory: %w", err)
	}
	if root.Mode()&fs.ModeSymlink != 0 || !root.IsDir() {
		return nil, fmt.Errorf("asset path %q must be a directory", source)
	}

	bundle := &Bundle{rootMode: root.Mode()}
	err = filepath.WalkDir(source, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == source {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("asset %q must not be a symlink", path)
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		switch {
		case info.IsDir():
			bundle.entries = append(bundle.entries, entry{path: rel, mode: info.Mode(), dir: true})
		case info.Mode().IsRegular():
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			bundle.entries = append(bundle.entries, entry{path: rel, mode: info.Mode(), data: data})
		default:
			return fmt.Errorf("asset %q must be a regular file or directory", path)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("load assets from %q: %w", source, err)
	}
	return bundle, nil
}

// Materialize writes the bundle beneath workspace/WorkspaceDir. Existing
// content at that path is a collision and is never replaced.
func (b *Bundle) Materialize(workspace string) (err error) {
	if b == nil {
		return nil
	}
	if workspace == "" {
		return errors.New("materialize goober assets: workspace is empty")
	}
	target := filepath.Join(workspace, WorkspaceDir)
	if _, statErr := os.Lstat(target); statErr == nil {
		return fmt.Errorf("%w: %s", ErrWorkspaceCollision, WorkspaceDir)
	} else if !errors.Is(statErr, fs.ErrNotExist) {
		return fmt.Errorf("inspect asset workspace path: %w", statErr)
	}

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
