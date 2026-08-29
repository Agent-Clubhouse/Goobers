package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// suiteWorktreeDir and suiteWorktreeStatus are TestMain's baseline for the
// #3459 guard: the package source directory the suite starts in, and its
// porcelain status before any test ran.
var (
	suiteWorktreeDir    string
	suiteWorktreeStatus string
)

// worktreeStatus returns `git status --porcelain` for dir — the same predicate
// test/ci's portal-dist-untracked gate uses, so both tracked modifications and
// stray untracked files are reported. It is scoped to dir (the package source
// directory) via an explicit "." pathspec, and returns an empty baseline when
// git is unavailable or dir is not inside a repository (a source tarball, a
// vendored checkout): the guard is a recurrence net, never a reason a suite
// cannot run at all.
func worktreeStatus(dir string) string {
	if dir == "" {
		return ""
	}
	out, err := exec.Command("git", "-C", dir, "status", "--porcelain", "--", ".").Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// worktreeStatusDrift reports the porcelain entries present in after but absent
// from before, sorted for a deterministic failure message. Comparing against a
// baseline (rather than requiring an outright clean tree) is what makes the
// guard usable: a contributor running the suite with their own work in progress
// sees only what THIS suite run wrote, not their own edits.
func worktreeStatusDrift(before, after string) []string {
	baseline := map[string]int{}
	for _, entry := range porcelainEntries(before) {
		baseline[entry]++
	}
	var drift []string
	for _, entry := range porcelainEntries(after) {
		if baseline[entry] > 0 {
			baseline[entry]--
			continue
		}
		drift = append(drift, entry)
	}
	sort.Strings(drift)
	return drift
}

func porcelainEntries(status string) []string {
	var entries []string
	for _, line := range strings.Split(status, "\n") {
		if strings.TrimSpace(line) != "" {
			entries = append(entries, line)
		}
	}
	return entries
}

// reportWorktreeDrift writes the #3459 guard's failure message to stderr and
// reports whether the suite dirtied the worktree. A test that writes into the
// repository tree instead of t.TempDir() leaves every contributor and every
// automated worktree with an unexplained modified (or untracked) file, and
// couples test execution to repository state; catching it here means the next
// such test fails loudly instead of silently reintroducing the condition.
func reportWorktreeDrift(dir, before string, stderr io.Writer) bool {
	drift := worktreeStatusDrift(before, worktreeStatus(dir))
	if len(drift) == 0 {
		return false
	}
	_, _ = io.WriteString(stderr, "cmd/goobers tests dirtied the worktree (#3459); "+
		"a test wrote into the repository tree instead of t.TempDir():\n"+
		strings.Join(drift, "\n")+"\n")
	return true
}

// TestWorktreeStatusDriftIgnoresPreexistingEntries covers the baseline
// comparison: work in progress a contributor already had in the tree is not
// reported as drift, while anything the suite itself added is.
func TestWorktreeStatusDriftIgnoresPreexistingEntries(t *testing.T) {
	before := " M cmd/goobers/apply.go\n?? scratch.txt\n"
	after := " M cmd/goobers/apply.go\n?? scratch.txt\n M cmd/goobers/mutations.jsonl\n"
	drift := worktreeStatusDrift(before, after)
	if len(drift) != 1 || drift[0] != " M cmd/goobers/mutations.jsonl" {
		t.Fatalf("drift = %q, want only the entry the suite added", drift)
	}
	if drift := worktreeStatusDrift(after, after); len(drift) != 0 {
		t.Fatalf("drift = %q, want none for an unchanged status", drift)
	}
	if drift := worktreeStatusDrift("", ""); len(drift) != 0 {
		t.Fatalf("drift = %q, want none for an empty status", drift)
	}
}

// TestReportWorktreeDriftDetectsFileWrittenIntoRepository is the #3459 guard's
// own regression test: a file written into a repository between the baseline
// and the check is reported, named, and flagged.
func TestReportWorktreeDriftDetectsFileWrittenIntoRepository(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "guard@example.com"},
		{"config", "user.name", "guard"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	before := worktreeStatus(dir)
	var clean strings.Builder
	if reportWorktreeDrift(dir, before, &clean) {
		t.Fatalf("clean repository reported drift: %s", clean.String())
	}
	if err := os.WriteFile(filepath.Join(dir, "mutations.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write stray file: %v", err)
	}
	var dirty strings.Builder
	if !reportWorktreeDrift(dir, before, &dirty) {
		t.Fatal("stray file written into the repository was not reported as drift")
	}
	if !strings.Contains(dirty.String(), "mutations.jsonl") {
		t.Fatalf("report = %q, want it to name the stray file", dirty.String())
	}
}

// TestWorktreeGuardArmedForSuite asserts TestMain captured the #3459 baseline,
// so the post-run check can actually report a test that writes into the
// repository tree. Without the baseline the guard silently degrades to a no-op.
func TestWorktreeGuardArmedForSuite(t *testing.T) {
	if suiteWorktreeDir == "" {
		t.Fatal("suiteWorktreeDir is empty — TestMain did not capture the #3459 worktree baseline")
	}
	if _, err := os.Stat(filepath.Join(suiteWorktreeDir, "worktreeclean_test.go")); err != nil {
		t.Fatalf("suiteWorktreeDir = %q, want the package source directory: %v", suiteWorktreeDir, err)
	}
}
