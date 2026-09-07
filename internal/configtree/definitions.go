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
// A shared root must be a real directory, not a symlink to unrelated content.
func WalkDefinitionTrees(root string, walk func(string) error) error {
	if err := walk(root); err != nil {
		return err
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	shared := filepath.Join(filepath.Dir(absolute), "goobers")
	if shared == absolute {
		return nil // A caller validating the shared tree itself already visited it.
	}
	info, err := os.Lstat(shared)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("shared goober root %s must be a directory, not a file or symlink", shared)
	}
	return walk(shared)
}
