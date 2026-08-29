package mergeresolve

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const manifest = `{
  "scripts": [
    "lint",
    "test"
  ]
}
`

func testGit(t *testing.T, dir string) Git {
	t.Helper()
	return func(args ...string) ([]byte, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		return cmd.Output()
	}
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newConflictedRepo builds a repository whose HEAD is mid-merge with one
// conflicted manifest: each side inserted its own script entry after "lint".
func newConflictedRepo(t *testing.T, theirs string) string {
	t.Helper()
	dir := t.TempDir()
	mustGit(t, dir, "init", "-q", "-b", "main")
	mustGit(t, dir, "config", "user.email", "test@example.com")
	mustGit(t, dir, "config", "user.name", "test")
	mustWrite(t, filepath.Join(dir, "package.json"), manifest)
	mustGit(t, dir, "add", ".")
	mustGit(t, dir, "commit", "-qm", "init")

	mustGit(t, dir, "checkout", "-q", "-b", "sibling")
	mustWrite(t, filepath.Join(dir, "package.json"), theirs)
	mustGit(t, dir, "commit", "-aqm", "sibling change")

	mustGit(t, dir, "checkout", "-q", "main")
	mustWrite(t, filepath.Join(dir, "package.json"),
		strings.Replace(manifest, `    "lint",`+"\n", `    "lint",`+"\n"+`    "package",`+"\n", 1))
	mustGit(t, dir, "commit", "-aqm", "own change")

	merge := exec.Command("git", "merge", "--no-edit", "sibling")
	merge.Dir = dir
	if out, err := merge.CombinedOutput(); err == nil {
		t.Fatalf("git merge unexpectedly succeeded: %s", out)
	}
	return dir
}

func TestResolveAdjacentLineConflictsMergesDistinctInsertions(t *testing.T) {
	dir := newConflictedRepo(t,
		strings.Replace(manifest, `    "lint",`+"\n", `    "lint",`+"\n"+`    "build",`+"\n", 1))

	status, err := ResolveAdjacentLineConflicts(dir, testGit(t, dir))
	if err != nil {
		t.Fatalf("ResolveAdjacentLineConflicts: %v", err)
	}
	if status != StatusResolved {
		t.Fatalf("status = %v, want StatusResolved", status)
	}
	merged, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range []string{`"build",`, `"package",`} {
		if !strings.Contains(string(merged), entry) {
			t.Fatalf("merged manifest = %q, want entry %s retained", merged, entry)
		}
	}
	files, err := UnmergedFiles(testGit(t, dir))
	if err != nil {
		t.Fatalf("UnmergedFiles: %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("unmerged files after resolution = %v, want none", files)
	}
}

func TestResolveAdjacentLineConflictsRefusesDivergentEdit(t *testing.T) {
	dir := newConflictedRepo(t, strings.Replace(manifest, `"lint",`, `"lint --fix",`, 1))

	status, err := ResolveAdjacentLineConflicts(dir, testGit(t, dir))
	if err != nil {
		t.Fatalf("ResolveAdjacentLineConflicts: %v", err)
	}
	if status != StatusUnsafe {
		t.Fatalf("status = %v, want StatusUnsafe", status)
	}
	files, err := UnmergedFiles(testGit(t, dir))
	if err != nil {
		t.Fatalf("UnmergedFiles: %v", err)
	}
	if len(files) != 1 || files[0].Path != "package.json" {
		t.Fatalf("unmerged files = %v, want the manifest left conflicted", files)
	}
}

func TestResolveAdjacentLineConflictsWithoutConflictIsAbsent(t *testing.T) {
	dir := t.TempDir()
	mustGit(t, dir, "init", "-q", "-b", "main")
	mustGit(t, dir, "config", "user.email", "test@example.com")
	mustGit(t, dir, "config", "user.name", "test")
	mustWrite(t, filepath.Join(dir, "package.json"), manifest)
	mustGit(t, dir, "add", ".")
	mustGit(t, dir, "commit", "-qm", "init")

	status, err := ResolveAdjacentLineConflicts(dir, testGit(t, dir))
	if err != nil {
		t.Fatalf("ResolveAdjacentLineConflicts: %v", err)
	}
	if status != StatusAbsent {
		t.Fatalf("status = %v, want StatusAbsent", status)
	}
}
