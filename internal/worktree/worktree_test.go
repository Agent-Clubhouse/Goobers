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

// TestManager_Create_ExcludesHarnessScratch is #240's regression guard: the
// harness scratch dir (.goobers/) written into a provisioned run worktree must
// be invisible to git — a `git add -A && commit` (the common agent pattern)
// captures none of it — even though the target repo has no .goobers entry in
// its own .gitignore.
func TestManager_Create_ExcludesHarnessScratch(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t) // foreign repo: no .goobers in its .gitignore (it has none)
	m := newTestManager(t)

	wt, err := m.Create(ctx, CreateOptions{
		RepoURL: repo, RunID: "run-1", BaseRef: "main", Branch: "goobers/impl/run-1",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// The harness materializes its scratch dir into the workspace; the agent
	// also makes a real change.
	mustWriteFile(t, filepath.Join(wt.Path, ".goobers", "prompt.md"), "the full prompt")
	mustWriteFile(t, filepath.Join(wt.Path, ".goobers", "result.json"), "{}")
	mustWriteFile(t, filepath.Join(wt.Path, ".goobers", "context", "blob"), "materialized context")
	mustWriteFile(t, filepath.Join(wt.Path, "src.txt"), "real implementation change")

	// (a) status shows the real change but not the scratch dir.
	status := runTestGit(t, wt.Path, "status", "--porcelain")
	if strings.Contains(status, ".goobers") {
		t.Fatalf("git status leaks harness scratch:\n%s", status)
	}
	if !strings.Contains(status, "src.txt") {
		t.Fatalf("git status should still show the real change:\n%s", status)
	}

	// (b) `git add -A && commit` captures no .goobers/ paths.
	runTestGit(t, wt.Path, "add", "-A")
	runTestGit(t, wt.Path, "-c", "user.email=t@e.test", "-c", "user.name=t", "commit", "-m", "impl")
	committed := runTestGit(t, wt.Path, "show", "--name-only", "--pretty=format:", "HEAD")
	if strings.Contains(committed, ".goobers") {
		t.Fatalf("committed tree contains harness scratch:\n%s", committed)
	}
	if !strings.Contains(committed, "src.txt") {
		t.Fatalf("committed tree should contain the real change:\n%s", committed)
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
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

// TestManager_Create_ResolvesRelativeRootToAbsolute is #282's regression: a
// Manager constructed with a relative Root (the common case — cmd/goobers
// wires it off a "."-rooted instance) must not let git resolve a worktree's
// destination path against the wrong subprocess's cwd. Before the fix,
// Manager.Root stayed relative (resolved against whatever the daemon/CLI
// process's cwd happened to be at construction time), and runGit's
// cmd.Dir = repoDir made git resolve that same relative destination against
// the managed mirror instead — silently nesting the real worktree inside
// repoDir/<relative-root>/... instead of at the flat path every later step
// (bot-identity config, the stage's own exec via cmd.Dir = wt.Path) expects.
// A t.Chdir into a fresh temp dir before constructing the Manager reproduces
// exactly how cmd/goobers wires this for a "."-rooted instance.
func TestManager_Create_ResolvesRelativeRootToAbsolute(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)

	t.Chdir(t.TempDir())
	m, err := NewManager("workcopies")
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if !filepath.IsAbs(m.Root) {
		t.Fatalf("Manager.Root = %q, want an absolute path", m.Root)
	}

	wt, err := m.Create(ctx, CreateOptions{RepoURL: repo, RunID: "run-1", BaseRef: "main"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !filepath.IsAbs(wt.Path) {
		t.Fatalf("Worktree.Path = %q, want an absolute path (not resolved against some subprocess's cwd)", wt.Path)
	}
	// The real proof: the worktree is actually populated at the flat path the
	// rest of the runner assumes — this is exactly where the live #282 repro
	// failed (bot-identity `git config` chdir'd into wt.Path and got
	// "no such file or directory" because the real checkout landed nested
	// inside repoDir instead).
	if _, err := os.Stat(filepath.Join(wt.Path, "README.md")); err != nil {
		t.Fatalf("expected README.md in the real worktree location %q: %v", wt.Path, err)
	}
}

// TestManager_Create_SetsLocalBotIdentity is #237's fix: an implementer
// stage commits inside its worktree, and that commit must not depend on the
// daemon host's own ambient git config (HOME/global gitconfig) — Create sets
// user.name/user.email local to the worktree's own config so `git commit`
// succeeds even with no global identity configured anywhere.
func TestManager_Create_SetsLocalBotIdentity(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // no ~/.gitconfig — proves the identity is local, not inherited
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "does-not-exist.gitconfig"))

	ctx := context.Background()
	repo := newSourceRepo(t)
	m := newTestManager(t)

	wt, err := m.Create(ctx, CreateOptions{RepoURL: repo, RunID: "run-1", BaseRef: "main"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	gotName := strings.TrimSpace(runTestGit(t, wt.Path, "config", "user.name"))
	if gotName != botGitUserName {
		t.Fatalf("user.name = %q, want %q", gotName, botGitUserName)
	}
	gotEmail := strings.TrimSpace(runTestGit(t, wt.Path, "config", "user.email"))
	if gotEmail != botGitUserEmail {
		t.Fatalf("user.email = %q, want %q", gotEmail, botGitUserEmail)
	}

	// Prove it's actually usable: a commit succeeds with no ambient identity.
	if err := os.WriteFile(filepath.Join(wt.Path, "change.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, wt.Path, "add", "change.txt")
	runTestGit(t, wt.Path, "commit", "-m", "no ambient identity needed")
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

	// Simulate a crash: force processAlive to report this marker's PID as
	// dead. A real reaped-subprocess PID is inherently racy against PID
	// recycling on a busy machine (issue #142's QA-gate stress flake) — the
	// injected fake makes "dead" deterministic instead.
	const fakeDeadPID = 999999
	prev := processAlive
	processAlive = func(pid int) bool { return pid != fakeDeadPID }
	t.Cleanup(func() { processAlive = prev })

	mk, err := readMarker(m.markerPath(wt.key, wt.RunID))
	if err != nil {
		t.Fatalf("readMarker: %v", err)
	}
	mk.PID = fakeDeadPID
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

func TestManager_Reap_RemovesDeregisteredMarkerlessDirectory(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	m := newTestManager(t)

	wt, err := m.Create(ctx, CreateOptions{RepoURL: repo, RunID: "deregistered", BaseRef: "main"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := os.Remove(m.markerPath(wt.key, wt.RunID)); err != nil {
		t.Fatalf("remove marker: %v", err)
	}
	if registered, err := worktreeRegistered(ctx, m.repoDirForKey(wt.key), wt.Path); err != nil || !registered {
		t.Fatalf("worktreeRegistered before deregistration = %v, %v; want true, nil", registered, err)
	}

	// Recreate the directory after Git removes and deregisters the worktree,
	// matching an interrupted removal that leaves only filesystem state behind.
	if err := runGit(ctx, m.repoDirForKey(wt.key), "worktree", "remove", "--force", wt.Path); err != nil {
		t.Fatalf("remove worktree registration: %v", err)
	}
	mustWriteFile(t, filepath.Join(wt.Path, "leftover.txt"), "leftover")
	if registered, err := worktreeRegistered(ctx, m.repoDirForKey(wt.key), wt.Path); err != nil || registered {
		t.Fatalf("worktreeRegistered after deregistration = %v, %v; want false, nil", registered, err)
	}

	results, warnings, err := m.Reap(ctx, ReapOptions{})
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected Reap warnings: %+v", warnings)
	}
	if len(results) != 1 || results[0].RunID != wt.RunID || results[0].Reason != ReapReasonMarkerless {
		t.Fatalf("unexpected Reap results: %+v", results)
	}
	if _, err := os.Stat(wt.Path); !os.IsNotExist(err) {
		t.Fatalf("expected deregistered worktree directory removed, stat err = %v", err)
	}

	results, warnings, err = m.Reap(ctx, ReapOptions{})
	if err != nil || len(results) != 0 || len(warnings) != 0 {
		t.Fatalf("second Reap should be a no-op, got %+v, warnings %+v, err %v", results, warnings, err)
	}
}

func TestWorktreeRegistered_SupportsGitBefore236(t *testing.T) {
	binDir := t.TempDir()
	fakeGit := filepath.Join(binDir, "git")
	script := `#!/bin/sh
for arg in "$@"; do
	if [ "$arg" = "-z" ]; then
		echo "unknown switch z" >&2
		exit 129
	fi
done
printf 'worktree %s\nHEAD deadbeef\n\n' "$FAKE_WORKTREE_PATH"
`
	if err := os.WriteFile(fakeGit, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	worktreePath := filepath.Join(t.TempDir(), "registered worktree")
	t.Setenv("FAKE_WORKTREE_PATH", worktreePath)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	registered, err := worktreeRegistered(context.Background(), t.TempDir(), worktreePath)
	if err != nil {
		t.Fatalf("worktreeRegistered with pre-2.36 Git: %v", err)
	}
	if !registered {
		t.Fatal("worktreeRegistered with pre-2.36 Git = false, want true")
	}
}

// TestManager_SafeBareRepositoryExplicit_StillWorks is #247's regression: a
// hardened `git config safe.bareRepository=explicit` (an increasingly common
// security default) makes git refuse cwd-based discovery of a bare repo,
// which is exactly how every call here reaches a managed mirror (cmd.Dir set
// to the mirror, no --git-dir/GIT_DIR). Without bareRepoSafeArgs's
// `-c safe.bareRepository=all` override, WorkingCopy/Create/Remove would all
// fail under this setting. GIT_CONFIG_GLOBAL simulates the hardened machine
// without mutating the real user/global git config.
func TestManager_SafeBareRepositoryExplicit_StillWorks(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	m := newTestManager(t)

	hardenedConfig := filepath.Join(t.TempDir(), "gitconfig-hardened")
	if err := os.WriteFile(hardenedConfig, []byte("[safe]\n\tbareRepository = explicit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", hardenedConfig)

	if _, err := m.WorkingCopy(ctx, repo); err != nil {
		t.Fatalf("WorkingCopy under safe.bareRepository=explicit: %v", err)
	}

	wt, err := m.Create(ctx, CreateOptions{RepoURL: repo, RunID: "hardened-run", BaseRef: "main"})
	if err != nil {
		t.Fatalf("Create under safe.bareRepository=explicit: %v", err)
	}
	if err := wt.Remove(ctx, RemoveOptions{}); err != nil {
		t.Fatalf("Remove under safe.bareRepository=explicit: %v", err)
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

// TestWorktree_Diff is #301: Worktree.Diff returns the unified diff of the run
// branch against its base, computed from the actual commits — the runner-owned
// evidence the reviewer gate judges (no model self-reporting). A branch with no
// commits vs. base diffs empty.
func TestWorktree_Diff(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	m := newTestManager(t)

	wt, err := m.Create(ctx, CreateOptions{
		RepoURL: repo, RunID: "run-diff", BaseRef: "main", Branch: "goobers/impl/run-diff",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// No commits on the branch yet → empty diff vs. base.
	empty, err := wt.Diff(ctx, "main")
	if err != nil {
		t.Fatalf("Diff (no commits): %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("expected an empty diff before any commit, got:\n%s", empty)
	}

	// Commit a real change on the run branch (Create already set a local bot
	// identity, so a plain commit works).
	mustWriteFile(t, filepath.Join(wt.Path, "feature.go"), "package feature\n\nfunc Added() {}\n")
	runTestGit(t, wt.Path, "add", "-A")
	runTestGit(t, wt.Path, "commit", "-m", "add feature")

	diff, err := wt.Diff(ctx, "main")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	got := string(diff)
	if !strings.Contains(got, "feature.go") {
		t.Fatalf("diff missing the changed file path:\n%s", got)
	}
	if !strings.Contains(got, "func Added()") {
		t.Fatalf("diff missing the added content:\n%s", got)
	}
	if !strings.Contains(got, "+package feature") {
		t.Fatalf("diff is not a unified add diff:\n%s", got)
	}
}

// TestWorktree_Diff_RequiresBaseRef guards the empty-baseRef fail-closed path.
func TestWorktree_Diff_RequiresBaseRef(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	m := newTestManager(t)
	wt, err := m.Create(ctx, CreateOptions{RepoURL: repo, RunID: "run-nobase", BaseRef: "main", Branch: "goobers/impl/run-nobase"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := wt.Diff(ctx, ""); err == nil {
		t.Fatal("expected an error for an empty baseRef, got nil")
	}
}
