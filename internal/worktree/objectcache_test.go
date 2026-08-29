package worktree

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// dirSize sums apparent file sizes under root, for comparing a dependent
// mirror's on-disk footprint against a full clone's.
func dirSize(t *testing.T, root string) int64 {
	t.Helper()
	var total int64
	err := filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return total
}

// TestManager_WorkingCopy_ObjectCacheSecondMirrorReusesCache is #654's core
// acceptance criterion: a second Manager (a second gaggle, in production)
// targeting the same repo URL creates a mirror that borrows objects from the
// shared cache instead of paying for its own full clone.
func TestManager_WorkingCopy_ObjectCacheSecondMirrorReusesCache(t *testing.T) {
	ctx := context.Background()
	repoDir := newSourceRepo(t)
	// A repo with real object weight, so a referencing mirror's disk usage
	// is a measurably small fraction of a full clone's, not just "smaller by
	// a few bytes of packfile overhead."
	for i := 0; i < 20; i++ {
		advanceOrigin(t, repoDir, "bulk-"+strconv.Itoa(i)+".txt")
	}
	// file:// forces the packfile transport: a plain local path clone takes
	// git's --local hardlink shortcut, which copies every object regardless
	// of --reference and would make this comparison meaningless (see
	// partialclone_test.go's testFileURL doc for the same reasoning).
	repo := testFileURL(t, repoDir)
	sharedRoot := t.TempDir()

	// Baseline: a Manager with no object cache measures a genuine full
	// clone's disk usage. Note the FIRST cache-enabled gaggle also benefits
	// from --reference (ensureObjectCache clones the cache before the
	// dependent mirror clones, even the very first time), so the baseline
	// must come from a separate, uncached Manager — "first vs second
	// cache-enabled gaggle" would not isolate the size effect at all.
	baseline, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	baselineMirror, err := baseline.WorkingCopy(ctx, repo)
	if err != nil {
		t.Fatalf("baseline (uncached) WorkingCopy: %v", err)
	}

	first, err := NewManager(t.TempDir(), WithPinnedRoot(sharedRoot), WithObjectCache())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.WorkingCopy(ctx, repo); err != nil {
		t.Fatalf("first gaggle WorkingCopy: %v", err)
	}

	second, err := NewManager(t.TempDir(), WithPinnedRoot(sharedRoot), WithObjectCache())
	if err != nil {
		t.Fatal(err)
	}
	secondMirror, err := second.WorkingCopy(ctx, repo)
	if err != nil {
		t.Fatalf("second gaggle WorkingCopy: %v", err)
	}

	key := repoKey(repo)
	cacheDir := filepath.Join(sharedRoot, objectCacheDirName, key)
	if _, err := os.Stat(cacheDir); err != nil {
		t.Fatalf("stat shared cache dir: %v", err)
	}
	alternates, err := readAlternates(filepath.Join(secondMirror, alternatesRelPath))
	if err != nil {
		t.Fatalf("read second mirror's alternates: %v", err)
	}
	wantObjectsDir := filepath.Clean(filepath.Join(cacheDir, "objects"))
	if resolved, err := filepath.EvalSymlinks(wantObjectsDir); err == nil {
		wantObjectsDir = resolved
	}
	found := false
	for _, alt := range alternates {
		if alt == wantObjectsDir {
			found = true
		}
	}
	if !found {
		t.Fatalf("second mirror's alternates = %v, want an entry for %s", alternates, wantObjectsDir)
	}

	// Compare objects/ specifically, not the whole mirror directory: a bare
	// mirror carries fixed per-repo overhead (hook templates, config,
	// packed-refs, description) that --reference/alternates was never meant
	// to shrink and that would otherwise dilute the comparison for a small
	// test fixture where that fixed overhead isn't negligible next to the
	// actual object data.
	baselineObjectsSize := dirSize(t, filepath.Join(baselineMirror, "objects"))
	secondObjectsSize := dirSize(t, filepath.Join(secondMirror, "objects"))
	if secondObjectsSize >= baselineObjectsSize/2 {
		t.Fatalf("second (referencing) mirror objects/ = %d bytes, uncached full-clone baseline objects/ = %d bytes — want the referencing mirror's own object storage to be a small fraction, not comparable", secondObjectsSize, baselineObjectsSize)
	}

	// A worktree cut from the referencing mirror must still see every
	// object — alternates make objects available for reads transparently.
	wt, err := second.Create(ctx, CreateOptions{RepoURL: repo, RunID: "run-1", BaseRef: "main", Branch: "goobers/wf/run-1"})
	if err != nil {
		t.Fatalf("Create from referencing mirror: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt.Path, "bulk-19.txt")); err != nil {
		t.Fatalf("worktree from referencing mirror missing bulk-19.txt: %v", err)
	}
}

// TestManager_WorkingCopy_ObjectCacheFsckClean is #654's fsck acceptance
// criterion: a mirror created with --reference, and a worktree cut from it,
// must both pass `git fsck` cleanly — an alternates-based reference must not
// leave either half-formed or missing-connectivity.
func TestManager_WorkingCopy_ObjectCacheFsckClean(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	m, err := NewManager(t.TempDir(), WithObjectCache())
	if err != nil {
		t.Fatal(err)
	}
	mirror, err := m.WorkingCopy(ctx, repo)
	if err != nil {
		t.Fatalf("WorkingCopy: %v", err)
	}
	if out := runTestGit(t, mirror, "fsck", "--full"); strings.TrimSpace(out) != "" {
		t.Fatalf("git fsck on dependent mirror reported issues:\n%s", out)
	}

	wt, err := m.Create(ctx, CreateOptions{RepoURL: repo, RunID: "run-1", BaseRef: "main", Branch: "goobers/wf/run-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if out := runTestGit(t, wt.Path, "fsck", "--full"); strings.TrimSpace(out) != "" {
		t.Fatalf("git fsck on worktree from referencing mirror reported issues:\n%s", out)
	}
}

// TestManager_WorkingCopy_ObjectCacheRefreshesOnMirrorRefresh pins "fetched
// on the same cadence as mirror refresh" (design §3 B3): a second
// WorkingCopy call for an already-cloned mirror also re-fetches the shared
// cache, so new commits are visible to a later gaggle sharing the cache
// without needing its own from-scratch clone.
func TestManager_WorkingCopy_ObjectCacheRefreshesOnMirrorRefresh(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	sharedRoot := t.TempDir()

	first, err := NewManager(t.TempDir(), WithPinnedRoot(sharedRoot), WithObjectCache())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.WorkingCopy(ctx, repo); err != nil {
		t.Fatalf("first gaggle WorkingCopy (clone): %v", err)
	}

	advanceOrigin(t, repo, "later.txt")

	// Refreshing the FIRST gaggle's mirror also refreshes the shared cache —
	// no new gaggle needs to be created to observe this.
	if _, err := first.WorkingCopy(ctx, repo); err != nil {
		t.Fatalf("first gaggle WorkingCopy (refresh): %v", err)
	}

	key := repoKey(repo)
	cacheDir := filepath.Join(sharedRoot, objectCacheDirName, key)
	head := strings.TrimSpace(runTestGit(t, repo, "rev-parse", "main"))
	cacheHead := strings.TrimSpace(runTestGit(t, cacheDir, "rev-parse", "refs/heads/main"))
	if cacheHead != head {
		t.Fatalf("cache HEAD = %q after origin advanced and the mirror refreshed, want %q — cache did not refresh on the same cadence", cacheHead, head)
	}
}

// TestManager_WorkingCopy_ObjectCacheInterruptedCloneLeavesNoCacheDir is
// #654's crash-safety acceptance criterion, mirroring the existing mirror
// clone-failure precedent (workingCopy's own "don't leave a partial clone
// masquerading as a valid one").
func TestManager_WorkingCopy_ObjectCacheInterruptedCloneLeavesNoCacheDir(t *testing.T) {
	ctx := context.Background()
	m, err := NewManager(t.TempDir(), WithObjectCache())
	if err != nil {
		t.Fatal(err)
	}
	badURL := "file:///nonexistent/definitely-not-a-repo-" + t.Name()

	if _, err := m.WorkingCopy(ctx, badURL); err == nil {
		t.Fatal("WorkingCopy succeeded against a nonexistent remote")
	}

	key := repoKey(badURL)
	cacheDir := filepath.Join(m.pinnedRoot, objectCacheDirName, key)
	if _, err := os.Stat(cacheDir); !os.IsNotExist(err) {
		t.Fatalf("stat cache dir after interrupted clone: err = %v, want IsNotExist", err)
	}
}

// TestManager_WorkingCopy_ObjectCacheOffCreatesNoObjectsDir is #654's
// flag-off regression criterion: with the option unset, WorkingCopy behaves
// exactly as before — no _objects directory is ever created.
func TestManager_WorkingCopy_ObjectCacheOffCreatesNoObjectsDir(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	root := t.TempDir()
	m, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.WorkingCopy(ctx, repo); err != nil {
		t.Fatalf("WorkingCopy (clone): %v", err)
	}
	if _, err := m.WorkingCopy(ctx, repo); err != nil {
		t.Fatalf("WorkingCopy (fetch): %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, objectCacheDirName)); !os.IsNotExist(err) {
		t.Fatalf("_objects dir stat = %v, want IsNotExist — flag off must never create it", err)
	}
}

// TestManager_WorkingCopy_ObjectCacheOffIsByteIdentical is the same
// byte-identical-git-invocation pin PartialClone's own flag-off test uses:
// with the option unset, WorkingCopy's clone/fetch invocations are unchanged.
func TestManager_WorkingCopy_ObjectCacheOffIsByteIdentical(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses a POSIX git shim")
	}
	ctx := context.Background()
	repo := newSourceRepo(t)
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	log := installRecordingGitShim(t)
	mirror, err := m.WorkingCopy(ctx, repo)
	if err != nil {
		t.Fatalf("WorkingCopy (clone): %v", err)
	}

	recorded := recordedGitLines(t, log)
	wantClone := hardenedGitPrefix + " clone --mirror " + repo + " " + mirror
	if got := findRecordedLine(recorded, " clone "); got != wantClone {
		t.Errorf("flag-off clone invocation:\n got %q\nwant %q", got, wantClone)
	}
	for _, line := range recorded {
		if strings.Contains(line, "--reference") {
			t.Errorf("flag-off path issued a --reference invocation: %q", line)
		}
	}
}

// TestManager_WorkingCopy_ObjectCacheOnInvocations pins the flag-on clone
// invocation shape: --reference to the cache directory, alongside the
// existing mirror-clone arguments.
func TestManager_WorkingCopy_ObjectCacheOnInvocations(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses a POSIX git shim")
	}
	ctx := context.Background()
	repo := newSourceRepo(t)
	root := t.TempDir()
	m, err := NewManager(root, WithObjectCache())
	if err != nil {
		t.Fatal(err)
	}

	log := installRecordingGitShim(t)
	mirror, err := m.WorkingCopy(ctx, repo)
	if err != nil {
		t.Fatalf("WorkingCopy (clone): %v", err)
	}

	cacheDir := filepath.Join(root, objectCacheDirName, repoKey(repo))
	recorded := recordedGitLines(t, log)
	wantClone := hardenedGitPrefix + " clone --mirror --reference " + cacheDir + " " + repo + " " + mirror
	if got := findRecordedLine(recorded, " clone --mirror --reference "); got != wantClone {
		t.Errorf("flag-on clone invocation:\n got %q\nwant %q", got, wantClone)
	}
	wantCacheClone := hardenedGitPrefix + " clone --mirror " + repo + " " + cacheDir
	if got := findRecordedLine(recorded, " clone --mirror "+repo+" "+cacheDir); got != wantCacheClone {
		t.Errorf("cache clone invocation:\n got %q\nwant %q", got, wantCacheClone)
	}
}

// TestManager_GCObjectCache_RefusesWithDependents is #654's fail-closed GC
// acceptance criterion: a cache with a live dependent mirror must not be
// deleted, and the refusal must name the dependent.
func TestManager_GCObjectCache_RefusesWithDependents(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	root := t.TempDir()
	m, err := NewManager(root, WithObjectCache())
	if err != nil {
		t.Fatal(err)
	}
	mirror, err := m.WorkingCopy(ctx, repo)
	if err != nil {
		t.Fatalf("WorkingCopy: %v", err)
	}

	result, err := m.GCObjectCache(repo, []string{root})
	if err != nil {
		t.Fatalf("GCObjectCache: %v", err)
	}
	if result.Deleted {
		t.Fatal("GCObjectCache deleted a cache with a live dependent")
	}
	if len(result.Dependents) != 1 || result.Dependents[0] != mirror {
		t.Fatalf("Dependents = %v, want exactly [%s]", result.Dependents, mirror)
	}

	key := repoKey(repo)
	cacheDir := filepath.Join(root, objectCacheDirName, key)
	if _, err := os.Stat(cacheDir); err != nil {
		t.Fatalf("cache dir removed despite a refused GC: %v", err)
	}
}

// TestManager_GCObjectCache_SucceedsAtZeroDependents is the GC helper's
// companion acceptance criterion: with every dependent mirror gone, GC
// deletes the cache.
func TestManager_GCObjectCache_SucceedsAtZeroDependents(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	root := t.TempDir()
	m, err := NewManager(root, WithObjectCache())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.WorkingCopy(ctx, repo); err != nil {
		t.Fatalf("WorkingCopy: %v", err)
	}
	key := repoKey(repo)
	mirrorDir := m.repoDirForKey(key)
	if err := os.RemoveAll(mirrorDir); err != nil {
		t.Fatalf("remove dependent mirror: %v", err)
	}

	result, err := m.GCObjectCache(repo, []string{root})
	if err != nil {
		t.Fatalf("GCObjectCache: %v", err)
	}
	if !result.Deleted {
		t.Fatalf("GCObjectCache did not delete a cache with zero dependents, dependents = %v", result.Dependents)
	}
	cacheDir := filepath.Join(root, objectCacheDirName, key)
	if _, err := os.Stat(cacheDir); !os.IsNotExist(err) {
		t.Fatalf("cache dir stat after successful GC: err = %v, want IsNotExist", err)
	}
}

// TestManager_GCObjectCache_NoCacheIsNotAnError covers GC against a repo URL
// that was never cached — a no-op, not a failure.
func TestManager_GCObjectCache_NoCacheIsNotAnError(t *testing.T) {
	repo := newSourceRepo(t)
	root := t.TempDir()
	m, err := NewManager(root, WithObjectCache())
	if err != nil {
		t.Fatal(err)
	}
	result, err := m.GCObjectCache(repo, []string{root})
	if err != nil {
		t.Fatalf("GCObjectCache: %v", err)
	}
	if result.Deleted || len(result.Dependents) != 0 {
		t.Fatalf("result = %+v, want a no-op", result)
	}
}

// TestManager_GCObjectCache_ScansEveryWorkcopiesRoot confirms the GC helper
// checks every root it is given, not just the first — the multi-gaggle
// shape the feature exists for.
func TestManager_GCObjectCache_ScansEveryWorkcopiesRoot(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	sharedRoot := t.TempDir()
	gaggleARoot := t.TempDir()
	gaggleBRoot := t.TempDir()

	a, err := NewManager(gaggleARoot, WithPinnedRoot(sharedRoot), WithObjectCache())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.WorkingCopy(ctx, repo); err != nil {
		t.Fatalf("gaggle A WorkingCopy: %v", err)
	}
	b, err := NewManager(gaggleBRoot, WithPinnedRoot(sharedRoot), WithObjectCache())
	if err != nil {
		t.Fatal(err)
	}
	bMirror, err := b.WorkingCopy(ctx, repo)
	if err != nil {
		t.Fatalf("gaggle B WorkingCopy: %v", err)
	}

	// Gaggle A's own mirror is gone; only gaggle B still depends on the cache.
	if err := os.RemoveAll(a.repoDirForKey(repoKey(repo))); err != nil {
		t.Fatal(err)
	}

	result, err := a.GCObjectCache(repo, []string{gaggleARoot, gaggleBRoot})
	if err != nil {
		t.Fatalf("GCObjectCache: %v", err)
	}
	if result.Deleted {
		t.Fatal("GCObjectCache deleted a cache gaggle B still depends on")
	}
	if len(result.Dependents) != 1 || result.Dependents[0] != bMirror {
		t.Fatalf("Dependents = %v, want exactly [%s] (gaggle B's mirror)", result.Dependents, bMirror)
	}
}
