package interactiveexec

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBrowserStoreRootsArePlatformScopedAndAliasesDeduplicate(t *testing.T) {
	home := t.TempDir()
	for _, relative := range []string{"Library/Application Support/Google/Chrome", "Library/Safari", "Library/Keychains", ".config/chromium", ".mozilla", ".local/share/keyrings"} {
		if err := os.MkdirAll(filepath.Join(home, relative), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, platform := range []string{"darwin", "linux"} {
		boundaries, err := browserBoundaries(home, platform)
		if err != nil || len(boundaries) != 3 {
			t.Fatalf("%s boundaries=%d err=%v", platform, len(boundaries), err)
		}
		for _, boundary := range boundaries {
			if len(boundary.Allow) != 0 || !filepath.IsAbs(boundary.Root) {
				t.Fatal("browser root gained an agent read exception")
			}
		}
	}
	boundaries, err := browserBoundaries(t.TempDir(), "darwin")
	if err != nil || len(boundaries) != 0 {
		t.Fatal("absent browser installation prevented startup", err)
	}
	root := filepath.Join(home, ".mozilla")
	boundaries = addBrowserBoundary(addBrowserBoundary(nil, root), filepath.Join(root, "profile"))
	boundaries = addBrowserBoundary(boundaries, root)
	if len(boundaries) != 1 {
		t.Fatal("overlapping profile roots were retained")
	}
}

func TestBrowserStoreInvalidRootFailsClosed(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".mozilla"), []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := browserBoundaries(home, "linux"); err == nil {
		t.Fatal("invalid browser root silently skipped")
	}
}
