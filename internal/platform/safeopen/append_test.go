package safeopen

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAppendAtPreservesExistingBytes(t *testing.T) {
	root := t.TempDir()
	dir, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()
	for _, text := range []string{"first\n", "second\n"} {
		f, err := AppendAt(dir, "receipt")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString(text); err != nil {
			t.Fatal(err)
		}
		if err := f.Sync(); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(filepath.Join(root, "receipt"))
	if err != nil || string(data) != "first\nsecond\n" {
		t.Fatalf("append contents = %q, %v", data, err)
	}
	for _, name := range []string{"", ".", "..", "../outside", "nested/file", "nested\\file", "receipt:stream"} {
		if f, err := AppendAt(dir, name); err == nil {
			_ = f.Close()
			t.Fatalf("unsafe child name accepted: %q", name)
		}
	}
}

func TestAppendAtRejectsLinkedTargets(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "directory"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			target := filepath.Join(t.TempDir(), "outside")
			if err := os.WriteFile(target, []byte("untouched"), 0o600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "receipt")
			var err error
			switch kind {
			case "symlink":
				err = os.Symlink(target, path)
			case "hardlink":
				err = os.Link(target, path)
			case "directory":
				err = os.Mkdir(path, 0o700)
			}
			if err != nil {
				t.Skipf("%s unavailable: %v", kind, err)
			}
			dir, err := Open(root)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = dir.Close() }()
			if f, err := AppendAt(dir, "receipt"); err == nil {
				_ = f.Close()
				t.Fatal("unsafe receipt target accepted")
			}
			data, err := os.ReadFile(target)
			if err != nil || string(data) != "untouched" {
				t.Fatalf("outside target changed: %q %v", data, err)
			}
		})
	}
}
