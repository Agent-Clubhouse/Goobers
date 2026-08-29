package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// packageDirGuardSentinel is the file whose presence identifies the current
// directory as this package's own source directory. The suite re-execs its
// own test binary as stage subprocesses whose working directory is a scratch
// worktree they may legitimately write into (mutation sidecars, result
// files), so the guard only arms itself where writing is actually illegal.
const packageDirGuardSentinel = "testmain_test.go"

// packageDirGuard is #3459's recurrence guard: a test must write its scratch
// output under t.TempDir(), never into the checked-out package directory. A
// test that writes here (a CLI entrypoint invoked without t.Chdir, whose
// worktree-relative sidecar lands in the repository) leaves the worktree
// dirty, which is unexplained noise for a contributor and a diff an automated
// worktree can end up committing. The guard compares a snapshot of the
// package directory taken before the suite runs against one taken after, so
// it reports the offending path regardless of whether the file is tracked,
// and — unlike a `git status --porcelain` over the same directory — never
// false-fails on a developer's own uncommitted edits.
type packageDirGuard struct {
	dir    string
	before map[string]string
}

// newPackageDirGuard snapshots dir when it is this package's source
// directory, and returns a disarmed guard otherwise.
func newPackageDirGuard(dir string) (*packageDirGuard, error) {
	if _, err := os.Stat(filepath.Join(dir, packageDirGuardSentinel)); err != nil {
		return &packageDirGuard{}, nil
	}
	before, err := snapshotDirTree(dir)
	if err != nil {
		return nil, err
	}
	return &packageDirGuard{dir: dir, before: before}, nil
}

// changes lists every path the suite added, removed, or rewrote under the
// guarded directory, sorted for a deterministic failure message.
func (g *packageDirGuard) changes() []string {
	if g == nil || g.dir == "" {
		return nil
	}
	after, err := snapshotDirTree(g.dir)
	if err != nil {
		return []string{fmt.Sprintf("re-scan %s: %v", g.dir, err)}
	}
	return diffDirSnapshots(g.before, after)
}

// snapshotDirTree fingerprints every file under dir by size and modification
// time — enough to catch a rewrite, including one that happens to restore
// byte-identical content, without reading 9 MB of committed assets.
func snapshotDirTree(dir string) (map[string]string, error) {
	snapshot := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		snapshot[filepath.ToSlash(relative)] = fmt.Sprintf("%d:%d", info.Size(), info.ModTime().UnixNano())
		return nil
	})
	if err != nil {
		return nil, err
	}
	return snapshot, nil
}

func diffDirSnapshots(before, after map[string]string) []string {
	var changes []string
	for path, fingerprint := range after {
		previous, existed := before[path]
		switch {
		case !existed:
			changes = append(changes, "created "+path)
		case previous != fingerprint:
			changes = append(changes, "modified "+path)
		}
	}
	for path := range before {
		if _, ok := after[path]; !ok {
			changes = append(changes, "removed "+path)
		}
	}
	sort.Strings(changes)
	return changes
}

func packageDirGuardFailure(dir string, changes []string) string {
	return fmt.Sprintf("test run wrote into the package directory %s: %s\n"+
		"tests must write scratch output under t.TempDir() (t.Chdir(t.TempDir()) for a "+
		"CLI entrypoint that writes worktree-relative files); writing here leaves the "+
		"worktree dirty (#3459)", dir, strings.Join(changes, ", "))
}

func TestPackageDirGuardReportsWritesIntoTheGuardedDirectory(t *testing.T) {
	dir := t.TempDir()
	writeGuardFixture(t, filepath.Join(dir, packageDirGuardSentinel), "package main")
	writeGuardFixture(t, filepath.Join(dir, "rewritten.txt"), "before")
	writeGuardFixture(t, filepath.Join(dir, "nested", "removed.txt"), "gone")

	guard, err := newPackageDirGuard(dir)
	if err != nil {
		t.Fatalf("newPackageDirGuard: %v", err)
	}

	writeGuardFixture(t, filepath.Join(dir, "rewritten.txt"), "after")
	writeGuardFixture(t, filepath.Join(dir, "mutations.jsonl"), "{}\n")
	if err := os.Remove(filepath.Join(dir, "nested", "removed.txt")); err != nil {
		t.Fatal(err)
	}

	want := []string{"created mutations.jsonl", "modified rewritten.txt", "removed nested/removed.txt"}
	if got := guard.changes(); !reflect.DeepEqual(got, want) {
		t.Fatalf("changes = %#v, want %#v", got, want)
	}
	if message := packageDirGuardFailure(dir, guard.changes()); !strings.Contains(message, "mutations.jsonl") ||
		!strings.Contains(message, "t.TempDir()") {
		t.Fatalf("failure message = %q, want the offending path and the remedy", message)
	}
}

func TestPackageDirGuardStaysQuietForAnUntouchedDirectory(t *testing.T) {
	dir := t.TempDir()
	writeGuardFixture(t, filepath.Join(dir, packageDirGuardSentinel), "package main")
	writeGuardFixture(t, filepath.Join(dir, "testdata", "fixture.json"), "{}\n")

	guard, err := newPackageDirGuard(dir)
	if err != nil {
		t.Fatalf("newPackageDirGuard: %v", err)
	}
	if changes := guard.changes(); len(changes) != 0 {
		t.Fatalf("changes = %#v, want none", changes)
	}
}

// TestPackageDirGuardDisarmsOutsideThePackageDirectory covers the stage
// worktrees this suite re-execs its own binary into: those processes write
// worktree-relative files (mutation sidecars, stage result files) legally, so
// the guard must not arm itself there.
func TestPackageDirGuardDisarmsOutsideThePackageDirectory(t *testing.T) {
	dir := t.TempDir()
	guard, err := newPackageDirGuard(dir)
	if err != nil {
		t.Fatalf("newPackageDirGuard: %v", err)
	}
	writeGuardFixture(t, filepath.Join(dir, mutationsSidecarFile), "{}\n")
	if changes := guard.changes(); len(changes) != 0 {
		t.Fatalf("changes = %#v, want none for an unguarded directory", changes)
	}
}

func writeGuardFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
