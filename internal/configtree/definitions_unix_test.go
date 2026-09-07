//go:build !windows

package configtree

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWalkDefinitionTreesRejectsSymlinkedSharedRoot(t *testing.T) {
	parent := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(parent, "goobers")); err != nil {
		t.Fatal(err)
	}
	visits := 0
	err := WalkDefinitionTrees(filepath.Join(parent, "config"), func(string) error {
		visits++
		return nil
	})
	if err == nil || visits != 1 {
		t.Fatalf("shared symlink followed: visits=%d, err=%v", visits, err)
	}
}
