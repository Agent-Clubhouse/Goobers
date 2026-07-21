package worktree

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The tests in this file cover the Windows git/worktree audit (#643). They run
// on every CI matrix leg. The Windows-specific behaviors — symlink flattening
// (core.symlinks=false) and long-path support — are driven deterministically on
// any platform through the Manager's injection seams (symlinkFallback + lstat),
// so a real Windows runner is not required to exercise them; when the Windows
// matrix leg (#633) lands, the same tests additionally assert the real behavior
// with no injection (see the runtime.GOOS branch in the symlink test).

// newSourceRepoWithSymlink builds a throwaway repo whose HEAD commit contains a
// regular file, a symlink to it, and a nested directory — enough to exercise
// symlink detection and a non-trivial checked-out path. Returns the repo path.
func newSourceRepoWithSymlink(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		// Creating the fixture's committed symlink needs symlink support; on a
		// Windows host without it the fixture itself cannot be built. The
		// injected-fallback assertions below do not depend on this fixture.
		t.Skip("fixture creation requires symlink support")
	}
	dir := t.TempDir()
	runTestGit(t, dir, "init", "-b", "main")
	runTestGit(t, dir, "config", "user.email", "test@example.com")
	runTestGit(t, dir, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(dir, "target.txt"), []byte("real contents\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target.txt", filepath.Join(dir, "link.txt")); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, dir, "add", ".")
	runTestGit(t, dir, "commit", "-m", "init")
	return dir
}

// TestManager_WorkingCopy_SetsManagedGitConfig asserts the worktree layer pins
// the deterministic git config (#643) on the mirror it creates: core.autocrlf
// off (no line-ending rewriting) and core.longpaths on (Windows >260-char
// paths). These are read by every worktree branched from the mirror.
func TestManager_WorkingCopy_SetsManagedGitConfig(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	m := newTestManager(t)

	dir, err := m.WorkingCopy(ctx, repo)
	if err != nil {
		t.Fatalf("WorkingCopy: %v", err)
	}
	for _, want := range []struct{ key, value string }{
		{"core.autocrlf", "false"},
		{"core.longpaths", "true"},
	} {
		got := strings.TrimSpace(runTestGit(t, dir, "config", "--get", want.key))
		if got != want.value {
			t.Errorf("mirror %s = %q, want %q", want.key, got, want.value)
		}
	}
}

// TestManager_WorkingCopy_ManagedConfigSelfHeals proves the config is (re)applied
// on the fetch path too, so a mirror cloned before this policy existed — or one
// an operator tampered with — is repaired on its next use, mirroring the
// idempotent ensureScratchExcluded contract.
func TestManager_WorkingCopy_ManagedConfigSelfHeals(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	m := newTestManager(t)

	dir, err := m.WorkingCopy(ctx, repo)
	if err != nil {
		t.Fatalf("WorkingCopy (first): %v", err)
	}
	// Simulate a pre-policy / tampered mirror: drop the settings.
	runTestGit(t, dir, "config", "--unset", "core.autocrlf")
	runTestGit(t, dir, "config", "--unset", "core.longpaths")

	// A second WorkingCopy takes the existing-mirror fetch path.
	if _, err := m.WorkingCopy(ctx, repo); err != nil {
		t.Fatalf("WorkingCopy (second): %v", err)
	}
	if got := strings.TrimSpace(runTestGit(t, dir, "config", "--get", "core.autocrlf")); got != "false" {
		t.Errorf("core.autocrlf after refetch = %q, want \"false\" (self-heal failed)", got)
	}
	if got := strings.TrimSpace(runTestGit(t, dir, "config", "--get", "core.longpaths")); got != "true" {
		t.Errorf("core.longpaths after refetch = %q, want \"true\" (self-heal failed)", got)
	}
}

// TestManager_Create_CleanStatusInvariant is the cross-platform invariant from
// the audit: a fresh worktree checkout with no edits must report an empty
// `git status`. It guards against a line-ending or symlink policy that would
// make git see phantom modifications the moment a worktree is provisioned. Runs
// on ubuntu, macos, and (when #633 lands) windows.
func TestManager_Create_CleanStatusInvariant(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	m := newTestManager(t)

	wt, err := m.Create(ctx, CreateOptions{
		RepoURL: repo, RunID: "run-clean", BaseRef: "main", Branch: "goobers/impl/run-clean",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if status := strings.TrimSpace(runTestGit(t, wt.Path, "status", "--porcelain")); status != "" {
		t.Errorf("fresh checkout is not clean; git status --porcelain =\n%s", status)
	}
}

// TestFlattenedSymlinks unit-tests the pure classifier off Windows by injecting
// lstat results: only index symlinks that did NOT materialize as real symlinks
// (git wrote them as plain files) are reported; real symlinks and un-lstat-able
// paths are not.
func TestFlattenedSymlinks(t *testing.T) {
	root := "/repo"
	lstat := func(name string) (os.FileInfo, error) {
		switch name {
		case filepath.Join(root, filepath.FromSlash("flat.txt")):
			return fakeInfo{mode: 0}, nil // regular file → flattened
		case filepath.Join(root, filepath.FromSlash("nested/flat2.txt")):
			return fakeInfo{mode: 0}, nil // regular file → flattened
		case filepath.Join(root, filepath.FromSlash("real.txt")):
			return fakeInfo{mode: os.ModeSymlink}, nil // real symlink → fine
		default:
			return nil, os.ErrNotExist // missing → skipped
		}
	}
	got := flattenedSymlinks(root, []string{"flat.txt", "nested/flat2.txt", "real.txt", "gone.txt"}, lstat)
	want := []string{"flat.txt", "nested/flat2.txt"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("flattenedSymlinks = %v, want %v", got, want)
	}
}

// TestSymlinkIndexEntries checks the git-plumbing parse against a real fixture:
// only the index entry stored as a symlink (mode 120000) is returned, not the
// regular file. Independent of how the checkout materialized (unix keeps the
// symlink real; the index mode is the same either way).
func TestSymlinkIndexEntries(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepoWithSymlink(t)
	m := newTestManager(t)
	wt, err := m.Create(ctx, CreateOptions{
		RepoURL: repo, RunID: "run-idx", BaseRef: "main", Branch: "goobers/impl/run-idx",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	links, err := symlinkIndexEntries(ctx, wt.Path)
	if err != nil {
		t.Fatalf("symlinkIndexEntries: %v", err)
	}
	if len(links) != 1 || links[0] != "link.txt" {
		t.Errorf("symlinkIndexEntries = %v, want [link.txt]", links)
	}
}

// TestManager_Create_SymlinkFlatteningWarning drives the end-to-end warning
// path. On a symlink-fallback platform (Windows semantics, here forced via the
// injection seams) a symlink that checked out as a plain file surfaces as a
// Worktree warning; with native symlink support (the darwin/linux default) the
// same repo produces no warning.
func TestManager_Create_SymlinkFlatteningWarning(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepoWithSymlink(t)

	t.Run("fallback platform warns", func(t *testing.T) {
		m := newTestManager(t)
		m.symlinkFallback = true
		// Simulate the Windows checkout: git wrote the symlink as a plain file,
		// so lstat sees a regular file for it.
		m.lstat = func(name string) (os.FileInfo, error) {
			fi, err := os.Lstat(name)
			if err != nil {
				return nil, err
			}
			if strings.HasSuffix(name, "link.txt") {
				return fakeInfo{mode: fi.Mode() &^ os.ModeSymlink}, nil
			}
			return fi, nil
		}
		wt, err := m.Create(ctx, CreateOptions{
			RepoURL: repo, RunID: "run-warn", BaseRef: "main", Branch: "goobers/impl/run-warn",
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if len(wt.Warnings) != 1 {
			t.Fatalf("Warnings = %v, want exactly one symlink warning", wt.Warnings)
		}
		if !strings.Contains(wt.Warnings[0], "link.txt") || !strings.Contains(wt.Warnings[0], "symlink") {
			t.Errorf("warning does not name the flattened symlink: %q", wt.Warnings[0])
		}
	})

	t.Run("native symlink support does not warn", func(t *testing.T) {
		m := newTestManager(t) // symlinkFallback defaults to runtime.GOOS=="windows"
		if runtime.GOOS == "windows" {
			t.Skip("host lacks native symlink support; covered by the fallback subtest")
		}
		wt, err := m.Create(ctx, CreateOptions{
			RepoURL: repo, RunID: "run-nowarn", BaseRef: "main", Branch: "goobers/impl/run-nowarn",
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if len(wt.Warnings) != 0 {
			t.Errorf("Warnings = %v, want none on a platform with symlink support", wt.Warnings)
		}
	})
}

// TestManager_Create_NoSymlinksNoWarning confirms a repo without symlinks never
// warns even on a fallback platform — the scan is precise, not a blanket
// "Windows always warns".
func TestManager_Create_NoSymlinksNoWarning(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t) // README.md only, no symlinks
	m := newTestManager(t)
	m.symlinkFallback = true

	wt, err := m.Create(ctx, CreateOptions{
		RepoURL: repo, RunID: "run-plain", BaseRef: "main", Branch: "goobers/impl/run-plain",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(wt.Warnings) != 0 {
		t.Errorf("Warnings = %v, want none for a symlink-free repo", wt.Warnings)
	}
}

// fakeInfo is a minimal os.FileInfo whose Mode() is the only meaningful field —
// enough for flattenedSymlinks, which only inspects the symlink bit.
type fakeInfo struct{ mode fs.FileMode }

func (f fakeInfo) Name() string       { return "" }
func (f fakeInfo) Size() int64        { return 0 }
func (f fakeInfo) Mode() fs.FileMode  { return f.mode }
func (f fakeInfo) ModTime() time.Time { return time.Time{} }
func (f fakeInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeInfo) Sys() any           { return nil }
