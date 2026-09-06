package worktree

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// buildHangingGit compiles a trivial Go program that sleeps far longer than
// any test timeout and installs it as "git" (or "git.exe" on Windows) on a
// fresh PATH-only directory. This is deliberately platform-agnostic — no
// bash/PowerShell/testdep dependency — so the same test proves bounded
// return on every OS this repo's CI matrix runs, Windows included (#4325's
// own acceptance criteria: "Windows tests cover a deliberately blocked Git
// subprocess and verify bounded return/cancellation").
func buildHangingGit(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "hanginggit.go")
	source := "package main\n\nimport \"time\"\n\nfunc main() {\n\ttime.Sleep(time.Hour)\n}\n"
	if err := os.WriteFile(src, []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	name := "git"
	if runtime.GOOS == "windows" {
		name = "git.exe"
	}
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, name)
	cmd := exec.Command("go", "build", "-o", binPath, src)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fake hanging git: %v\n%s", err, out)
	}
	return binDir
}

func withHangingGitOnPath(t *testing.T) {
	t.Helper()
	binDir := buildHangingGit(t)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// buildSelectivelyHangingGit compiles a "git" stand-in that sleeps forever
// when ANY argument contains hangSubstring, and otherwise execs the real
// system git unchanged. This proves a git-subprocess timeout on ONE
// retained-worktree candidate does not stop every other candidate's cleanup
// from proceeding normally — the shape of #4325's "one cleanup timeout
// cannot indefinitely block ... reaping" acceptance criterion.
func buildSelectivelyHangingGit(t *testing.T, hangSubstring string) string {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("locate real git: %v", err)
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "selectivehanggit.go")
	source := `package main

import (
	"os"
	"os/exec"
	"strings"
	"time"
)

func main() {
	for _, arg := range os.Args[1:] {
		if strings.Contains(arg, "` + hangSubstring + `") {
			time.Sleep(time.Hour)
		}
	}
	cmd := exec.Command(os.Getenv("GOOBERS_TEST_REAL_GIT"), os.Args[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			os.Exit(exitErr.ExitCode())
		}
		os.Exit(1)
	}
}
`
	if err := os.WriteFile(src, []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	name := "git"
	if runtime.GOOS == "windows" {
		name = "git.exe"
	}
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, name)
	buildCmd := exec.Command("go", "build", "-o", binPath, src)
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build selectively-hanging git: %v\n%s", err, out)
	}
	t.Setenv("GOOBERS_TEST_REAL_GIT", realGit)
	return binDir
}

func withShortCleanupTimeouts(t *testing.T) {
	t.Helper()
	originalTimeout, originalWait := cleanupGitTimeout, cleanupKillWaitDelay
	cleanupGitTimeout = 200 * time.Millisecond
	cleanupKillWaitDelay = 200 * time.Millisecond
	t.Cleanup(func() { cleanupGitTimeout, cleanupKillWaitDelay = originalTimeout, originalWait })
}

// TestRunCleanupGitBoundsAHangingSubprocess covers #4325's core acceptance
// criterion: a git subprocess that never returns must not be able to hold
// retained-worktree cleanup — and therefore daemon startup, the scheduler
// heartbeat, or shutdown — hostage.
func TestRunCleanupGitBoundsAHangingSubprocess(t *testing.T) {
	withHangingGitOnPath(t)
	withShortCleanupTimeouts(t)

	start := time.Now()
	err := runCleanupGit(context.Background(), t.TempDir(), "worktree remove", "worktree", "remove", "--force", "x")
	elapsed := time.Since(start)

	var timeoutErr *GitCleanupTimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("runCleanupGit error = %v, want *GitCleanupTimeoutError", err)
	}
	if timeoutErr.Op != "worktree remove" {
		t.Errorf("Op = %q, want %q", timeoutErr.Op, "worktree remove")
	}
	if !timeoutErr.Timeout() {
		t.Error("Timeout() = false, want true")
	}
	if strings.Contains(timeoutErr.Error(), "token") || strings.Contains(timeoutErr.Error(), "://") {
		t.Errorf("error text looks like it leaked credential material: %q", timeoutErr.Error())
	}
	// A generous multiple of the (shrunk) timeout + kill-wait bounds: this
	// asserts "did not hang for anywhere near the real hour-long sleep,"
	// not a tight race against scheduler jitter.
	if bound := cleanupGitTimeout + cleanupKillWaitDelay + 5*time.Second; elapsed > bound {
		t.Fatalf("runCleanupGit took %s, want under %s; bounded wait did not engage", elapsed, bound)
	}
}

// TestRunCleanupGitOutputBoundsAHangingSubprocess covers the output-returning
// twin used by worktree-registration and branch-enumeration cleanup calls.
func TestRunCleanupGitOutputBoundsAHangingSubprocess(t *testing.T) {
	withHangingGitOnPath(t)
	withShortCleanupTimeouts(t)

	start := time.Now()
	_, err := runCleanupGitOutput(context.Background(), t.TempDir(), "worktree list", "worktree", "list", "--porcelain")
	elapsed := time.Since(start)

	var timeoutErr *GitCleanupTimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("runCleanupGitOutput error = %v, want *GitCleanupTimeoutError", err)
	}
	if bound := cleanupGitTimeout + cleanupKillWaitDelay + 5*time.Second; elapsed > bound {
		t.Fatalf("runCleanupGitOutput took %s, want under %s; bounded wait did not engage", elapsed, bound)
	}
}

// TestRunCleanupGitDistinguishesCallerCancellationFromItsOwnTimeout proves a
// caller-driven ctx cancellation (the caller gave up) returns ctx.Err()
// unchanged rather than being reported as this package's own timeout —
// callers need to tell the two apart to decide whether a git subprocess is
// worth retrying later.
func TestRunCleanupGitDistinguishesCallerCancellationFromItsOwnTimeout(t *testing.T) {
	withHangingGitOnPath(t)
	// Deliberately leave cleanupGitTimeout at its production default so the
	// outer cancellation fires first.
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	err := runCleanupGit(ctx, t.TempDir(), "worktree remove", "worktree", "remove", "--force", "x")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("runCleanupGit error = %v, want context.Canceled", err)
	}
	var timeoutErr *GitCleanupTimeoutError
	if errors.As(err, &timeoutErr) {
		t.Fatalf("runCleanupGit reported its own timeout %v for a caller-driven cancellation", timeoutErr)
	}
}

// TestRunCleanupGitSucceedsOnARealGitCommand proves the bounded-wait
// machinery does not change behavior on the ordinary, fast-returning path —
// only a subprocess that outlives cleanupGitTimeout is affected.
func TestRunCleanupGitSucceedsOnARealGitCommand(t *testing.T) {
	dir := t.TempDir()
	runTestGit(t, dir, "init", "-b", "main")

	out, err := runCleanupGitOutput(context.Background(), dir, "rev-parse", "rev-parse", "--is-bare-repository")
	if err != nil {
		t.Fatalf("runCleanupGitOutput: %v", err)
	}
	if out != "false" {
		t.Errorf("output = %q, want %q", out, "false")
	}
}

// TestManagerReapGitTimeoutOnOneOrphanDoesNotAbortOthers is #4325's
// end-to-end proof: a git subprocess timeout on one retained-worktree
// candidate is skipped and reported for retry, but every other candidate is
// still reaped normally in the same pass — the pass as a whole never
// aborts.
func TestManagerReapGitTimeoutOnOneOrphanDoesNotAbortOthers(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	m := newTestManager(t)

	stuck, err := m.Create(ctx, CreateOptions{RepoURL: repo, RunID: "stuck", BaseRef: "main"})
	if err != nil {
		t.Fatalf("Create(stuck): %v", err)
	}
	fine, err := m.Create(ctx, CreateOptions{RepoURL: repo, RunID: "fine", BaseRef: "main"})
	if err != nil {
		t.Fatalf("Create(fine): %v", err)
	}

	const fakeDeadPID = 999999
	prevAlive := processAlive
	processAlive = func(pid int) bool { return pid != fakeDeadPID }
	t.Cleanup(func() { processAlive = prevAlive })

	for _, wt := range []*Worktree{stuck, fine} {
		mk, err := readMarker(m.markerPath(wt.key, wt.RunID))
		if err != nil {
			t.Fatalf("readMarker(%s): %v", wt.RunID, err)
		}
		mk.PID = fakeDeadPID
		if err := writeMarker(m.markerPath(wt.key, wt.RunID), mk); err != nil {
			t.Fatalf("writeMarker(%s): %v", wt.RunID, err)
		}
	}

	binDir := buildSelectivelyHangingGit(t, filepath.Base(stuck.Path))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	// A generous (but still test-fast) timeout: the "fine" worktree's
	// removal now goes through two extra exec layers (this process's own
	// wrapper, which re-execs the real git), so it needs enough headroom to
	// comfortably finish under load — cleanupGitTimeout only needs to be
	// shorter than the artificial 1-hour hang, not razor-thin.
	originalTimeout, originalWait := cleanupGitTimeout, cleanupKillWaitDelay
	cleanupGitTimeout = 5 * time.Second
	cleanupKillWaitDelay = 500 * time.Millisecond
	t.Cleanup(func() { cleanupGitTimeout, cleanupKillWaitDelay = originalTimeout, originalWait })

	results, warnings, err := m.Reap(ctx, ReapOptions{})
	if err != nil {
		t.Fatalf("Reap: %v (a single git-subprocess timeout must not abort the whole pass)", err)
	}
	if len(results) != 1 || results[0].RunID != "fine" || results[0].Reason != ReapReasonOrphaned {
		t.Fatalf("Reap results = %+v, want exactly the fine orphan reaped", results)
	}
	if len(warnings) != 1 {
		t.Fatalf("Reap warnings = %+v, want exactly one (the stuck orphan's timeout)", warnings)
	}
	var timeoutErr *GitCleanupTimeoutError
	if !errors.As(warnings[0].Err, &timeoutErr) {
		t.Fatalf("Reap warning = %v, want a *GitCleanupTimeoutError", warnings[0].Err)
	}

	if _, err := os.Stat(fine.Path); !os.IsNotExist(err) {
		t.Fatalf("fine worktree should have been removed, stat err = %v", err)
	}
	if _, err := os.Stat(stuck.Path); err != nil {
		t.Fatalf("stuck worktree should still exist for a later retry, stat err = %v", err)
	}
}

// TestWorktreeRemoveBoundedOnHangingGit proves #4325's "run teardown" case:
// the ordinary end-of-run Worktree.Remove path — not just the batch
// reap/retention sweep — is bounded too, so a hanging git subprocess cannot
// hold a run's own teardown hostage either.
func TestWorktreeRemoveBoundedOnHangingGit(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	m := newTestManager(t)

	wt, err := m.Create(ctx, CreateOptions{RepoURL: repo, RunID: "teardown", BaseRef: "main"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	binDir := buildHangingGit(t)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	withShortCleanupTimeouts(t)

	start := time.Now()
	err = wt.Remove(ctx, RemoveOptions{})
	elapsed := time.Since(start)

	var timeoutErr *GitCleanupTimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("Remove error = %v, want a *GitCleanupTimeoutError", err)
	}
	if bound := cleanupGitTimeout + cleanupKillWaitDelay + 5*time.Second; elapsed > bound {
		t.Fatalf("Remove took %s, want under %s; bounded wait did not engage", elapsed, bound)
	}
}

// TestRunCleanupGitReportsRealGitFailures proves a genuine git failure
// (distinct from a timeout) still surfaces as the existing *gitCommandError
// shape callers already classify on (e.g. IsTransientProvisionError),
// unaffected by the new bounded-wait wrapper.
func TestRunCleanupGitReportsRealGitFailures(t *testing.T) {
	dir := t.TempDir()
	runTestGit(t, dir, "init", "-b", "main")

	err := runCleanupGit(context.Background(), dir, "branch delete", "branch", "-D", "does-not-exist")
	if err == nil {
		t.Fatal("runCleanupGit = nil, want an error deleting a nonexistent branch")
	}
	var timeoutErr *GitCleanupTimeoutError
	if errors.As(err, &timeoutErr) {
		t.Fatalf("runCleanupGit reported a timeout for an ordinary git failure: %v", err)
	}
	var gitErr *gitCommandError
	if !errors.As(err, &gitErr) {
		t.Fatalf("runCleanupGit error = %v (%T), want *gitCommandError", err, err)
	}
}
