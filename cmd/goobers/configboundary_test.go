package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/executor"
)

// gitRepoWithRunBranchChanges builds a temp git repo — a base commit on main,
// then a run branch that adds `files` — and returns the worktree dir checked out
// to that run branch. This mirrors what the runner hands the open-pr stage: a
// worktree on the run branch carrying the prior stages' committed changes (#133).
func gitRepoWithRunBranchChanges(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	git("config", "user.email", "tutor@example.test")
	git("config", "user.name", "tutor")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-q", "-m", "base")
	git("checkout", "-q", "-b", "goobers/tutor/run-1")
	for rel, content := range files {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("add", "-A")
	git("commit", "-q", "-m", "tutor change")
	return dir
}

// TestOpenPRWriteBoundaryRejectsOutOfRootChange is #223's core negative test,
// exercised through the REAL open-pr stage: with confinement on and the config
// root "selfhost", a run branch that touches a platform path is refused — the
// cycle fails closed and NO PR is opened.
func TestOpenPRWriteBoundaryRejectsOutOfRootChange(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "run-1")
	t.Setenv(executor.InputEnvVar("confineToConfigRoot"), "true")
	t.Setenv(executor.InputEnvVar("configRoot"), "selfhost")

	wt := gitRepoWithRunBranchChanges(t, map[string]string{
		"selfhost/gaggles/goobers/workflows/tutor.yaml": "kind: Workflow\n",
		"internal/runner/run.go":                        "// smuggled platform edit\n",
	})
	t.Chdir(wt)

	code, _, stderr := runArgs(t, "open-pr", root)
	if code != 1 {
		t.Fatalf("code = %d, want 1 (boundary rejects out-of-root change); stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "config write-boundary") {
		t.Fatalf("stderr = %q, want a config write-boundary error", stderr)
	}
	server.mu.Lock()
	n := len(server.prs)
	server.mu.Unlock()
	if n != 0 {
		t.Fatalf("opened %d PR(s); a boundary breach must open none", n)
	}
}

// TestOpenPRWriteBoundaryAllowsConfigOnlyChange: the mirror positive — a change
// confined to the config root passes and the PR is opened.
func TestOpenPRWriteBoundaryAllowsConfigOnlyChange(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "run-1")
	t.Setenv(executor.InputEnvVar("confineToConfigRoot"), "true")
	t.Setenv(executor.InputEnvVar("configRoot"), "selfhost")

	wt := gitRepoWithRunBranchChanges(t, map[string]string{
		"selfhost/gaggles/goobers/workflows/tutor.yaml":          "kind: Workflow\n",
		"selfhost/gaggles/goobers/goobers/coder/instructions.md": "# tutor guidance\n",
	})
	t.Chdir(wt)

	code, stdout, stderr := runArgs(t, "open-pr", root)
	if code != 0 {
		t.Fatalf("code = %d, want 0 (config-only change allowed); stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "pr #") {
		t.Fatalf("stdout = %q, want an opened PR", stdout)
	}
	server.mu.Lock()
	n := len(server.prs)
	server.mu.Unlock()
	if n != 1 {
		t.Fatalf("opened %d PR(s); want exactly 1", n)
	}
}

// TestOpenPRWriteBoundaryFailsClosedOnUnverifiableDiff: when confinement is
// requested but the diff can't be computed (CWD is not a git repo), the stage
// refuses rather than opening the PR unverified.
func TestOpenPRWriteBoundaryFailsClosedOnUnverifiableDiff(t *testing.T) {
	root := initDemo(t)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	providerCmdEnv(t, server, "GOOBERS_CRED_GITHUB_PR_WRITE", "run-1")
	t.Setenv(executor.InputEnvVar("confineToConfigRoot"), "true")
	t.Setenv(executor.InputEnvVar("configRoot"), "selfhost")
	t.Chdir(t.TempDir()) // not a git repo

	code, _, stderr := runArgs(t, "open-pr", root)
	if code != 1 {
		t.Fatalf("code = %d, want 1 (fail closed on unverifiable diff); stderr = %q", code, stderr)
	}
	if !strings.Contains(stderr, "config write-boundary") {
		t.Fatalf("stderr = %q, want a config write-boundary error", stderr)
	}
	server.mu.Lock()
	n := len(server.prs)
	server.mu.Unlock()
	if n != 0 {
		t.Fatalf("opened %d PR(s); an unverifiable diff must open none", n)
	}
}
