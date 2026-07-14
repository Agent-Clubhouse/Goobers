package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// newSourceRepo creates a throwaway git repo with one commit on "main" and
// returns its filesystem path, usable directly as a repoURL (git clones over
// local paths fine).
func newSourceRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runTestGit(t, dir, "init", "-b", "main")
	runTestGit(t, dir, "config", "user.email", "test@example.com")
	runTestGit(t, dir, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, dir, "add", ".")
	runTestGit(t, dir, "commit", "-m", "init")
	return dir
}

func runTestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestManager_Create_ChecksOutBaseRef(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	m := newTestManager(t)

	wt, err := m.Create(ctx, CreateOptions{RepoURL: repo, RunID: "run-1", BaseRef: "main"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt.Path, "README.md")); err != nil {
		t.Fatalf("expected README.md in worktree: %v", err)
	}
}

func TestManager_Create_Branch(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	m := newTestManager(t)

	wt, err := m.Create(ctx, CreateOptions{
		RepoURL: repo, RunID: "run-1", BaseRef: "main", Branch: "goobers/wf/run-1",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	got := strings.TrimSpace(runTestGit(t, wt.Path, "rev-parse", "--abbrev-ref", "HEAD"))
	if got != "goobers/wf/run-1" {
		t.Fatalf("HEAD branch = %q, want goobers/wf/run-1", got)
	}
}

// TestManager_Create_AdoptsAndResetsExistingKey is issue #136's fix: a
// leftover worktree at the same key (a never-torn-down previous attempt of
// the same run+stage — a crash mid-attempt, or a same-process retry whose
// own Remove call failed) must be cleared and recreated fresh, not refused
// forever. Two DIFFERENT concurrent attempts of the same (run, stage) key
// never happen in practice (see forceClear's doc comment), so this is safe.
func TestManager_Create_AdoptsAndResetsExistingKey(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	m := newTestManager(t)

	first, err := m.Create(ctx, CreateOptions{RepoURL: repo, RunID: "run-1", BaseRef: "main"})
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}
	if err := os.WriteFile(filepath.Join(first.Path, "marker-of-first-attempt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash: the first attempt's worktree was never torn down
	// (no Remove call at all — the never-torn-down leftover this fix targets).

	second, err := m.Create(ctx, CreateOptions{RepoURL: repo, RunID: "run-1", BaseRef: "main"})
	if err != nil {
		t.Fatalf("second Create should adopt-and-reset rather than error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(second.Path, "marker-of-first-attempt")); !os.IsNotExist(err) {
		t.Fatalf("expected a genuinely fresh worktree, but the first attempt's file survived: err = %v", err)
	}
	if err := second.Remove(ctx, RemoveOptions{}); err != nil {
		t.Fatalf("Remove: %v", err)
	}
}

func TestManager_Remove_TearsDown(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	m := newTestManager(t)

	wt, err := m.Create(ctx, CreateOptions{RepoURL: repo, RunID: "run-1", BaseRef: "main"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := wt.Remove(ctx, RemoveOptions{}); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(wt.Path); !os.IsNotExist(err) {
		t.Fatalf("expected worktree path removed, stat err = %v", err)
	}
	if _, err := os.Stat(m.markerPath(repoKey(repo), "run-1")); !os.IsNotExist(err) {
		t.Fatalf("expected marker removed, stat err = %v", err)
	}

	list := runTestGit(t, m.repoDirForKey(repoKey(repo)), "worktree", "list", "--porcelain")
	if strings.Contains(list, wt.Path) {
		t.Fatalf("git worktree list still shows removed worktree: %s", list)
	}
}

// TestManager_ConcurrentRuns_Isolated is the acceptance test from #16: two
// concurrent runs against one repo never see each other's changes.
func TestManager_ConcurrentRuns_Isolated(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	m := newTestManager(t)

	const n = 5
	var wg sync.WaitGroup
	errs := make([]error, n)
	worktrees := make([]*Worktree, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			runID := "run-" + string(rune('a'+i))
			wt, err := m.Create(ctx, CreateOptions{RepoURL: repo, RunID: runID, BaseRef: "main"})
			if err != nil {
				errs[i] = err
				return
			}
			marker := filepath.Join(wt.Path, "run.marker")
			if err := os.WriteFile(marker, []byte(runID), 0o644); err != nil {
				errs[i] = err
				return
			}
			worktrees[i] = wt
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}

	// Each worktree must see only its own marker file, never a sibling's.
	for i, wt := range worktrees {
		entries, err := os.ReadDir(wt.Path)
		if err != nil {
			t.Fatalf("run %d: read worktree dir: %v", i, err)
		}
		var markers []string
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".marker") {
				markers = append(markers, e.Name())
			}
		}
		if len(markers) != 1 {
			t.Fatalf("run %d: expected exactly 1 marker file in %s, got %v", i, wt.Path, markers)
		}
	}

	for i, wt := range worktrees {
		if err := wt.Remove(ctx, RemoveOptions{}); err != nil {
			t.Fatalf("run %d: Remove: %v", i, err)
		}
	}
}

func TestManager_WorkingCopy_ClonesThenFetches(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	m := newTestManager(t)

	dir1, err := m.WorkingCopy(ctx, repo)
	if err != nil {
		t.Fatalf("WorkingCopy (clone): %v", err)
	}

	// Commit directly into the source repo, then confirm WorkingCopy picks it
	// up on refresh (a push would be denied: git refuses to update a
	// non-bare repo's checked-out branch from a remote).
	if err := os.WriteFile(filepath.Join(repo, "second.txt"), []byte("more\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, repo, "add", ".")
	runTestGit(t, repo, "commit", "-m", "second")

	dir2, err := m.WorkingCopy(ctx, repo)
	if err != nil {
		t.Fatalf("WorkingCopy (fetch): %v", err)
	}
	if dir1 != dir2 {
		t.Fatalf("expected the same managed working copy dir, got %q and %q", dir1, dir2)
	}

	wt, err := m.Create(ctx, CreateOptions{RepoURL: repo, RunID: "after-fetch", BaseRef: "main"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt.Path, "second.txt")); err != nil {
		t.Fatalf("expected fetched commit visible in new worktree: %v", err)
	}
}

func TestManager_Reap_RemovesDeadProcessOrphan(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	m := newTestManager(t)

	wt, err := m.Create(ctx, CreateOptions{RepoURL: repo, RunID: "crashed", BaseRef: "main"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Simulate a crash: overwrite the marker with a PID that is guaranteed
	// dead (a short-lived subprocess we've already waited on).
	dead := deadPID(t)
	mk, err := readMarker(m.markerPath(wt.key, wt.RunID))
	if err != nil {
		t.Fatalf("readMarker: %v", err)
	}
	mk.PID = dead
	if err := writeMarker(m.markerPath(wt.key, wt.RunID), mk); err != nil {
		t.Fatalf("writeMarker: %v", err)
	}

	results, warnings, err := m.Reap(ctx, ReapOptions{})
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected Reap warnings: %+v", warnings)
	}
	if len(results) != 1 || results[0].RunID != "crashed" || results[0].Reason != ReapReasonOrphaned {
		t.Fatalf("unexpected Reap results: %+v", results)
	}
	if _, err := os.Stat(wt.Path); !os.IsNotExist(err) {
		t.Fatalf("expected orphaned worktree removed, stat err = %v", err)
	}
}

func TestManager_Reap_LeavesLiveRunAlone(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	m := newTestManager(t)

	wt, err := m.Create(ctx, CreateOptions{RepoURL: repo, RunID: "live", BaseRef: "main"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	results, warnings, err := m.Reap(ctx, ReapOptions{})
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected Reap warnings: %+v", warnings)
	}
	if len(results) != 0 {
		t.Fatalf("expected no reap results for a live run, got %+v", results)
	}
	if _, err := os.Stat(wt.Path); err != nil {
		t.Fatalf("expected live worktree untouched: %v", err)
	}
}

func TestManager_Reap_KeptWorktreeSurvivesUntilStale(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	m := newTestManager(t)

	wt, err := m.Create(ctx, CreateOptions{RepoURL: repo, RunID: "kept", BaseRef: "main"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := wt.Remove(ctx, RemoveOptions{Keep: true}); err != nil {
		t.Fatalf("Remove(Keep): %v", err)
	}

	// StaleAfter disabled: kept worktree is left alone regardless of age.
	if results, warnings, err := m.Reap(ctx, ReapOptions{}); err != nil || len(results) != 0 || len(warnings) != 0 {
		t.Fatalf("Reap with StaleAfter=0 should not touch kept worktrees, got %+v, warnings %+v, err %v", results, warnings, err)
	}
	if _, err := os.Stat(wt.Path); err != nil {
		t.Fatalf("expected kept worktree still present: %v", err)
	}

	// Backdate the marker so it reads as older than StaleAfter, without
	// sleeping in the test.
	mk, err := readMarker(m.markerPath(wt.key, wt.RunID))
	if err != nil {
		t.Fatalf("readMarker: %v", err)
	}
	mk.CreatedAt = time.Now().Add(-time.Hour)
	if err := writeMarker(m.markerPath(wt.key, wt.RunID), mk); err != nil {
		t.Fatalf("writeMarker: %v", err)
	}

	results, warnings, err := m.Reap(ctx, ReapOptions{StaleAfter: time.Minute})
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected Reap warnings: %+v", warnings)
	}
	if len(results) != 1 || results[0].RunID != "kept" || results[0].Reason != ReapReasonStale {
		t.Fatalf("unexpected Reap results: %+v", results)
	}
	if _, err := os.Stat(wt.Path); !os.IsNotExist(err) {
		t.Fatalf("expected stale kept worktree removed, stat err = %v", err)
	}
}

// TestManager_Reap_UnreadableMarkerDoesNotAbortPass is issue #136's
// fail-open fix: one corrupt marker must not prevent every other repo's (or
// even every other worktree in the SAME repo's) genuine orphans from being
// cleaned up.
func TestManager_Reap_UnreadableMarkerDoesNotAbortPass(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	m := newTestManager(t)

	// A genuine crash orphan that Reap must still clean up despite the
	// corrupt marker below.
	orphan, err := m.Create(ctx, CreateOptions{RepoURL: repo, RunID: "orphan", BaseRef: "main"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	dead := deadPID(t)
	mk, err := readMarker(m.markerPath(orphan.key, orphan.RunID))
	if err != nil {
		t.Fatalf("readMarker: %v", err)
	}
	mk.PID = dead
	if err := writeMarker(m.markerPath(orphan.key, orphan.RunID), mk); err != nil {
		t.Fatalf("writeMarker: %v", err)
	}

	// A second worktree in the same repo whose marker is corrupted (not
	// valid JSON) — simulating a torn write.
	corrupt, err := m.Create(ctx, CreateOptions{RepoURL: repo, RunID: "corrupt", BaseRef: "main"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	corruptMarkerPath := m.markerPath(corrupt.key, corrupt.RunID)
	if err := os.WriteFile(corruptMarkerPath, []byte("{not valid json"), 0o644); err != nil {
		t.Fatalf("corrupt marker: %v", err)
	}

	results, warnings, err := m.Reap(ctx, ReapOptions{})
	if err != nil {
		t.Fatalf("Reap returned an error instead of failing open: %v", err)
	}
	if len(warnings) != 1 || warnings[0].Path != corruptMarkerPath {
		t.Fatalf("expected exactly one warning for the corrupt marker, got %+v", warnings)
	}
	if len(results) != 1 || results[0].RunID != "orphan" || results[0].Reason != ReapReasonOrphaned {
		t.Fatalf("expected the orphan to still be reaped despite the corrupt marker: %+v", results)
	}
	if _, err := os.Stat(orphan.Path); !os.IsNotExist(err) {
		t.Fatalf("expected the orphan worktree removed, stat err = %v", err)
	}
	if _, err := os.Stat(corrupt.Path); err != nil {
		t.Fatalf("expected the corrupt-marker worktree left alone (not guessed at), stat err = %v", err)
	}
}

// TestManager_Reap_RemovesMarkerlessWorktree is issue #136's orphan-diff
// fix: a crash between `git worktree add` and Manager.Create's marker write
// leaves a worktree with no marker at all, invisible to a marker-driven scan
// unless Reap also diffs actual worktree directories against markers.
func TestManager_Reap_RemovesMarkerlessWorktree(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	m := newTestManager(t)

	wt, err := m.Create(ctx, CreateOptions{RepoURL: repo, RunID: "no-marker", BaseRef: "main"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Simulate the crash window: the worktree exists, but its marker never
	// got written.
	if err := os.Remove(m.markerPath(wt.key, wt.RunID)); err != nil {
		t.Fatalf("remove marker: %v", err)
	}

	results, warnings, err := m.Reap(ctx, ReapOptions{})
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected Reap warnings: %+v", warnings)
	}
	if len(results) != 1 || results[0].RunID != "no-marker" || results[0].Reason != ReapReasonMarkerless {
		t.Fatalf("unexpected Reap results: %+v", results)
	}
	if _, err := os.Stat(wt.Path); !os.IsNotExist(err) {
		t.Fatalf("expected the markerless worktree removed, stat err = %v", err)
	}
}

// deadPID spawns a trivial subprocess, waits for it to exit, and returns its
// PID — guaranteed not to be alive, without racing PID reuse in practice for
// the lifetime of a test.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("spawn short-lived process: %v", err)
	}
	return cmd.Process.Pid
}
