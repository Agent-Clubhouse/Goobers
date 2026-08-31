package workcopyroot

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	if err := Validate("workcopies.root", ""); err != nil {
		t.Fatalf("empty root: %v", err)
	}
	abs := filepath.Join(t.TempDir(), "copies")
	if err := Validate("workcopies.root", abs); err != nil {
		t.Fatalf("absolute root: %v", err)
	}
	err := Validate("spec.workcopies.root", filepath.Join("relative", "copies"))
	if err == nil {
		t.Fatal("expected a relative root to be rejected")
	}
	if !strings.Contains(err.Error(), "spec.workcopies.root must be an absolute path") {
		t.Fatalf("unexpected message: %v", err)
	}
}

func TestKeyNormalizes(t *testing.T) {
	root := t.TempDir()
	dirty := filepath.Join(root, "a", "..", "a", "b") + string(filepath.Separator)
	key, err := Key(dirty)
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	want := filepath.Join(root, "a", "b")
	if runtime.GOOS == "windows" {
		want = strings.ToLower(want)
	}
	if key != want {
		t.Fatalf("Key(%q) = %q, want %q", dirty, key, want)
	}
}

func TestOverlap(t *testing.T) {
	a := filepath.Join(string(filepath.Separator), "w", "alpha")
	nested := filepath.Join(a, "beta")
	sibling := filepath.Join(string(filepath.Separator), "w", "alpha-two")

	if !Overlap(a, a) {
		t.Fatal("identical directories must overlap")
	}
	if !Overlap(a, nested) || !Overlap(nested, a) {
		t.Fatal("nested directories must overlap in both directions")
	}
	if Overlap(a, sibling) {
		t.Fatalf("%q and %q must not overlap", a, sibling)
	}
}
