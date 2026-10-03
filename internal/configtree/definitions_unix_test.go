//go:build !windows

package configtree

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWalkDefinitionTreesRejectsSymlinkedSharedRoot(t *testing.T) {
	parent := t.TempDir()
	if err := os.Mkdir(filepath.Join(parent, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
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

func TestWalkDefinitionTreesRejectsSymlinkedPrimaryRootBeforeCallback(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "config")
	if err := os.Symlink(t.TempDir(), root); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{root, root + string(os.PathSeparator), filepath.Join(root, ".")} {
		t.Run(path, func(t *testing.T) {
			called := false
			err := WalkDefinitionTrees(path, func(string) error {
				called = true
				return nil
			})
			if err == nil {
				t.Fatal("primary symlink accepted as a directory")
			}
			if called {
				t.Fatal("walk callback invoked for primary symlink")
			}
		})
	}
}
