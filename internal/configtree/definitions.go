package configtree

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// WalkDefinitionTrees visits the config tree followed by its optional sibling
// goobers tree. Keeping the original config root at call sites preserves
// relative provenance (../goobers/name/goober.yaml) across all source consumers.
// Each root must be a real directory, not a symlink to unrelated content.
func WalkDefinitionTrees(root string, walk func(string) error) error {
	if err := requireDirectory(root, "config root"); err != nil {
		return err
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	if err := walk(root); err != nil {
		return err
	}
	shared := filepath.Join(filepath.Dir(absolute), "goobers")
	if shared == absolute {
		return nil // A caller validating the shared tree itself already visited it.
	}
	if err := requireDirectory(shared, "shared goober root"); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	return walk(shared)
}

func requireDirectory(path, label string) error {
	info, err := os.Lstat(filepath.Clean(path))
	if errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s %s must be a directory, not a file or symlink", label, path)
	}
	return nil
}
