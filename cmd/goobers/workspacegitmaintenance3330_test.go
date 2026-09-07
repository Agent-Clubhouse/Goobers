package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/testgit"
	"github.com/goobers/goobers/internal/worktree"
)

// #3330: a git command this package runs inside a MANAGED worktree writes into
// the mirror's object store, because a linked worktree shares it. Git answers a
// write like that with `git maintenance run --auto`, which it detaches by
// default; the orphan outlives the command this process waited on and keeps
// creating files under `repo.git` while the caller tears the mirror down —
// worktree.Reap and FinalizeRun in production, t.TempDir's RemoveAll in the
// suite, which is how the flake was reported ("unlinkat .../<key>: directory
// not empty", attributed to whichever test owned the temp dir).
//
// internal/worktree pinned the same two settings on its own git calls
// (#3990/#4000) and this package's calls stayed uncovered. They could not be
// covered by the environment either: composeGitEnv strips inherited
// GIT_CONFIG_* so its slot indices are unambiguous, which drops the suite's own
// disableGitAutoMaintenanceForTests before the child ever sees it — so the
// existing TestGitAutoMaintenanceDisabledForSuite passed (it reads the ambient
// environment) while every real call ran unpinned.

// gitCommandLineConfig reads the `-c key=value` pairs off a command's argv.
func gitCommandLineConfig(args []string) map[string]string {
	config := map[string]string{}
	for i := 0; i+1 < len(args); i++ {
		if args[i] != "-c" {
			continue
		}
		key, value, ok := strings.Cut(args[i+1], "=")
		if ok {
			config[strings.ToLower(key)] = value
		}
	}
	return config
}

// The argument half: every constructor this package's workspace git calls go
// through must carry the pin, on the COMMAND LINE, where composeGitEnv cannot
// strip it and no repository or inherited configuration can outrank it.
func TestWorkspaceGitCommandsPinAutoMaintenanceToTheForeground(t *testing.T) {
	const dir = "/workspace"
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"unauthenticated", workspaceGitCommand(dir, "rev-parse", "HEAD").Args},
		{"authenticated", workspaceGitAuthCommand(dir, "tok", "fetch", "https://example.test/r", "refs/heads/b").Args},
		{"authenticated env", workspaceGitAuthEnvCommand(dir, gitAuthEnv("tok"), "push", "https://example.test/r", "b:b").Args},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := gitCommandLineConfig(tc.args)
			for _, key := range []string{"maintenance.autodetach", "gc.autodetach"} {
				if got := config[key]; got != "false" {
					t.Errorf("%s = %q, want \"false\" in %v — without it git detaches "+
						"housekeeping that outlives this command and writes into the "+
						"mirror its caller is tearing down (#3330)", key, got, tc.args)
				}
			}
		})
	}
}

// The pin is worktree's definition, not a second copy of it: a package that
// re-declared the settings could drift out of agreement with the mirror's own
// hardening and leave exactly half the object store protected.
func TestWorkspaceGitPinIsTheWorktreeDefinition(t *testing.T) {
	got := strings.Join(hardenedWorkspaceGitArgs(nil), " ")
	want := strings.Join(worktree.ForegroundMaintenanceArgs(), " ")
	if got != want {
		t.Fatalf("hardenedWorkspaceGitArgs = %q, want worktree.ForegroundMaintenanceArgs %q", got, want)
	}
}

// `-c` is the one pre-verb option carrying a separate value argument, and that
// value does not start with a dash — so the push exemption in gitFailureFor,
// which reads the verb off argv, must not mistake it for the subcommand. A
// misread here relabels a forge's own protected-branch rejection as a
// workspace git fault and makes it non-retryable (#4106's failure mode).
func TestGitSubcommandReadsThroughTheMaintenancePin(t *testing.T) {
	push := workspaceGitAuthCommand("/workspace", "tok", "push", "--force-with-lease", "https://example.test/r", "b:b")
	if got := gitSubcommand(push); got != "push" {
		t.Fatalf("gitSubcommand = %q, want \"push\" from %v", got, push.Args)
	}
	err := gitFailureFor(push, fmt.Errorf("exit status 1"))
	if err == nil {
		t.Fatal("gitFailureFor returned nil for a failing push")
	}
	var tagged *gitCommandError
	if errors.As(err, &tagged) {
		t.Fatalf("a rejected push was tagged as a workspace git fault: %v", err)
	}
	fetch := workspaceGitAuthCommand("/workspace", "tok", "fetch", "https://example.test/r", "refs/heads/b")
	if got := gitSubcommand(fetch); got != "fetch" {
		t.Fatalf("gitSubcommand = %q, want \"fetch\" from %v", got, fetch.Args)
	}
}

// The reported failure, reproduced through the real path: rebase a managed
// worktree onto its base and remove the manager root straight away, the way
// t.TempDir's cleanup and Reap both do. The mirror asks for detached
// housekeeping and the origin is wide enough that a repack of the objects the
// rebase just wrote is worth starting and takes long enough to overlap the
// removal, so without the pin the walk and the writer overlap and RemoveAll
// fails with "directory not empty".
func TestManagedWorktreeRebaseLeavesNothingWritingInTheMirror(t *testing.T) {
	origin := newWideMaintenanceOrigin(t, "goobers/impl/run-3330")
	root := t.TempDir()
	manager, err := worktree.NewManager(root)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	wt, err := manager.Create(context.Background(), worktree.CreateOptions{
		RepoURL: origin, RunID: "run-3330", BaseRef: "main",
		Branch: "goobers/pr-remediation/run-3330",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, mirror := range mirrorGitDirs(t, root) {
		configureDetachedMaintenanceIn(t, mirror)
	}
	if _, err := checkoutExistingBranch(wt.Path, "goobers/impl/run-3330", "test-token"); err != nil {
		t.Fatalf("checkoutExistingBranch: %v", err)
	}
	if _, _, _, err := attemptRebase(wt.Path, "main", "test-token"); err != nil {
		t.Fatalf("attemptRebase: %v", err)
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatalf("remove the manager root straight after the rebase that wrote into it: %v — "+
			"a detached git maintenance process is still writing under the mirror (#3330)", err)
	}
}

// mirrorGitDirs returns every `repo.git` under a manager root.
func mirrorGitDirs(t *testing.T, root string) []string {
	t.Helper()
	var dirs []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() && filepath.Base(path) == "repo.git" {
			dirs = append(dirs, path)
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if len(dirs) == 0 {
		t.Fatalf("no repo.git mirror under %s; the fixture no longer exercises the shared object store", root)
	}
	return dirs
}

// configureDetachedMaintenanceIn asks a repository for the detached
// housekeeping git defaults to, and enables the tasks whose --auto conditions
// a freshly written object store meets. The command-line pin must win over all
// of it — that is the property under test.
func configureDetachedMaintenanceIn(t *testing.T, repository string) {
	t.Helper()
	for _, setting := range [][2]string{
		{"gc.autoDetach", "true"},
		{"maintenance.autoDetach", "true"},
		{"maintenance.loose-objects.enabled", "true"},
		{"maintenance.loose-objects.auto", "1"},
		{"maintenance.commit-graph.enabled", "true"},
	} {
		out, err := testgit.Command("-C", repository, "config", setting[0], setting[1]).CombinedOutput()
		if err != nil {
			t.Fatalf("git config %s in %s: %v\n%s", setting[0], repository, err, out)
		}
	}
}

// newWideMaintenanceOrigin builds an origin with a PR branch and enough blobs
// that a repack of them is worth starting and slow enough to overlap the
// removal that follows.
func newWideMaintenanceOrigin(t *testing.T, prBranch string) string {
	t.Helper()
	const blobs = 2000
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		full := append([]string{"-C", dir}, args...)
		if out, err := testgit.Command(full...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-b", "main")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "test")
	if err := os.MkdirAll(filepath.Join(dir, "blobs"), 0o755); err != nil {
		t.Fatal(err)
	}
	for i := range blobs {
		name := filepath.Join(dir, "blobs", fmt.Sprintf("blob-%04d.txt", i))
		if err := os.WriteFile(name, fmt.Appendf(nil, "blob %d\n", i), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "-q", "-m", "wide tree")
	run("checkout", "-q", "-b", prBranch)
	if err := os.WriteFile(filepath.Join(dir, "pr.txt"), []byte("from pr\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "pr.txt")
	run("commit", "-q", "-m", "pr commit")
	run("checkout", "-q", "main")
	if err := os.WriteFile(filepath.Join(dir, "base.txt"), []byte("from base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "base.txt")
	run("commit", "-q", "-m", "base commit")
	return dir
}
