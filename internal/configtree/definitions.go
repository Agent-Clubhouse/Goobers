package configtree

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// SharedGooberDirectory returns the sibling shared-goober root only for the
// recognized nested config/ layout. It returns an empty path for flat or
// unrelated roots.
func SharedGooberDirectory(root string) string {
	absolute, err := filepath.Abs(root)
	if err != nil || filepath.Base(absolute) != "config" {
		return ""
	}
	return filepath.Join(root, "..", "goobers")
}

// WalkDefinitionTrees visits the config tree followed by its optional sibling
// goobers tree when root is the recognized nested config/ layout. Keeping the
// original config root at call sites preserves relative provenance across all
// source consumers. Each root must be a real directory, not a symlink.
func WalkDefinitionTrees(root string, walk func(string) error) error {
	if err := requireDirectory(root, "config root"); err != nil {
		return err
	}
	if err := walk(root); err != nil {
		return err
	}
	shared := SharedGooberDirectory(root)
	if shared == "" {
		return nil
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
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s %s must be a directory, not a file or symlink", label, path)
	}
	return nil
}
