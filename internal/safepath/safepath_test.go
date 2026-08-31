package safepath

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveCreatesMissingIntermediates(t *testing.T) {
	root := t.TempDir()

	full, err := Resolve(root, filepath.Join(".goobers", "context", "00-input"), true)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if err := os.WriteFile(full, []byte("data"), 0o600); err != nil {
		t.Fatalf("write resolved path: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, ".goobers", "context", "00-input"))
	if err != nil || string(data) != "data" {
		t.Fatalf("read back = %q, err %v", data, err)
	}
}

func TestResolveRejectsMissingDirectoryWhenNotCreating(t *testing.T) {
	root := t.TempDir()

	if _, err := Resolve(root, filepath.Join("missing", "file"), false); err == nil {
		t.Fatal("expected Resolve to refuse a missing directory when createMissingDirs is false")
	}
}

func TestResolveRejectsEscapingPath(t *testing.T) {
	root := t.TempDir()

	if _, err := Resolve(root, filepath.Join("..", "outside"), true); err == nil {
		t.Fatal("expected Resolve to reject a path escaping the root")
	}
	if _, err := Resolve(root, "", true); err == nil {
		t.Fatal("expected Resolve to reject an empty path")
	}
}

// TestResolveRefusesToTraverseASymlinkedAncestor is the core no-follow
// guarantee: os.MkdirAll would happily walk through "link" and create the
// missing components on the other side of it.
func TestResolveRefusesToTraverseASymlinkedAncestor(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}

	if _, err := Resolve(root, filepath.Join("link", "new", "out.md"), true); err == nil {
		t.Fatal("expected Resolve to refuse to traverse a symlinked ancestor")
	}
	if _, err := os.Lstat(filepath.Join(outside, "new")); !os.IsNotExist(err) {
		t.Fatalf("directory was created outside the root through the symlink: err=%v", err)
	}
}

func TestResolveRefusesASymlinkedLeaf(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(outside, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "out.md")); err != nil {
		t.Fatal(err)
	}

	if _, err := Resolve(root, "out.md", true); err == nil {
		t.Fatal("expected Resolve to refuse a leaf that is already a symlink")
	}
	data, err := os.ReadFile(outside)
	if err != nil || string(data) != "original" {
		t.Fatalf("outside file was modified through the symlink: %q (err %v)", data, err)
	}
}

func TestMkdirLeafCreatesAndIsIdempotent(t *testing.T) {
	root := t.TempDir()
	rel := filepath.Join(".goobers", "sandbox", "copilot-home")

	full, err := MkdirLeaf(root, rel, 0o700)
	if err != nil {
		t.Fatalf("MkdirLeaf: %v", err)
	}
	info, err := os.Stat(full)
	if err != nil || !info.IsDir() {
		t.Fatalf("leaf directory missing: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("leaf permissions = %o, want 700", perm)
	}
	again, err := MkdirLeaf(root, rel, 0o700)
	if err != nil || again != full {
		t.Fatalf("MkdirLeaf on an existing directory = (%q, %v), want (%q, nil)", again, err, full)
	}
}

func TestMkdirLeafRejectsNonDirectoryAndSymlinkedAncestor(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := MkdirLeaf(root, "file", 0o700); err == nil {
		t.Fatal("expected MkdirLeaf to reject an existing non-directory leaf")
	}

	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := MkdirLeaf(root, filepath.Join("link", "runtime"), 0o700); err == nil {
		t.Fatal("expected MkdirLeaf to refuse to traverse a symlinked ancestor")
	}
	if _, err := os.Lstat(filepath.Join(outside, "runtime")); !os.IsNotExist(err) {
		t.Fatalf("directory was created outside the root through the symlink: err=%v", err)
	}
}
