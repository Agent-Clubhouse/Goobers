package safeopen

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenRegularFileAndDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(path, []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}

	f, err := Open(path)
	if err != nil {
		t.Fatalf("Open(regular file): %v", err)
	}
	_ = f.Close()

	d, err := Open(dir)
	if err != nil {
		t.Fatalf("Open(directory): %v", err)
	}
	_ = d.Close()
}

func TestOpenMissingIsNotExist(t *testing.T) {
	_, err := Open(filepath.Join(t.TempDir(), "absent"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Open(missing) error = %v, want fs.ErrNotExist", err)
	}
	if errors.Is(err, ErrSymlink) {
		t.Fatalf("a missing path must not report as a symlink: %v", err)
	}
}

func TestOpenRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.txt")
	if err := os.WriteFile(target, []byte("real"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	_, err := Open(link)
	if !errors.Is(err, ErrSymlink) {
		t.Fatalf("Open(symlink) error = %v, want ErrSymlink", err)
	}
}

func TestOpenAtOpensChildAndRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "child.txt"), []byte("c"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "child.txt")
	if err := os.Symlink(target, filepath.Join(root, "evil")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}

	dir, err := Open(root)
	if err != nil {
		t.Fatalf("Open(root): %v", err)
	}
	defer func() { _ = dir.Close() }()

	child, err := OpenAt(dir, "child.txt")
	if err != nil {
		t.Fatalf("OpenAt(child): %v", err)
	}
	_ = child.Close()

	if _, err := OpenAt(dir, "evil"); !errors.Is(err, ErrSymlink) {
		t.Fatalf("OpenAt(symlink) error = %v, want ErrSymlink", err)
	}
}
