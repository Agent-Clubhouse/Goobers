package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #5653: another process on the host holding the mirror's gc lock is benign
// and self-clearing, so the opportunistic maintenance pass that follows a
// refresh must skip rather than fail the caller's WorkingCopy/Create — in
// production it discarded a successful agentic implement one second later.
func TestWorkingCopySkipsMaintenanceWhileAnotherGCHoldsTheMirror(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	m := newTestManager(t)
	dir, err := m.WorkingCopy(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}

	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	// git gc treats a fresh gc.pid naming this host and a live pid as a
	// concurrent gc; the test process is that live pid.
	mustWriteFile(t, filepath.Join(dir, "gc.pid"), fmt.Sprintf("%d %s", os.Getpid(), host))
	if out := runTestGitAllowFailure(t, dir, "maintenance", "run"); !strings.Contains(out, "gc is already running") {
		t.Skipf("precondition: this git/host did not report a held gc lock from gc.pid (output %q)", out)
	}

	if _, err := m.WorkingCopy(ctx, repo); err != nil {
		t.Fatalf("WorkingCopy with another gc holding the mirror: %v — maintenance lock contention must be skipped, not fatal (#5653)", err)
	}
	if _, err := m.Create(ctx, CreateOptions{RepoURL: repo, RunID: "run-gc", BaseRef: "main"}); err != nil {
		t.Fatalf("Create with another gc holding the mirror: %v (#5653)", err)
	}
}

func TestIsMaintenanceLockContention(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		output string
		want   bool
	}{
		{
			name:   "gc already running",
			output: "fatal: gc is already running on machine 'host-1' pid 916251 (use --force if not)\nerror: task 'gc' failed\n",
			want:   true,
		},
		{
			name:   "packed-refs lock held",
			output: "fatal: Unable to create '/w/repo.git/packed-refs.lock': File exists.\n\nAnother git process seems to be running in this repository",
			want:   true,
		},
		{
			name:   "corrupt object store",
			output: "error: object file objects/ab/cdef is empty\nfatal: bad object HEAD\nerror: task 'gc' failed\n",
		},
		{
			name:   "disk full",
			output: "fatal: unable to write new index file: No space left on device\n",
		},
		{
			name: "untyped error",
			err:  errors.New("gc is already running"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.err
			if err == nil {
				err = &gitCommandError{args: []string{"maintenance", "run"}, cause: errors.New("exit status 1"), output: []byte(tc.output), exitCode: 1}
			}
			if got := isMaintenanceLockContention(fmt.Errorf("wrapped: %w", err)); got != tc.want {
				t.Fatalf("isMaintenanceLockContention = %v, want %v", got, tc.want)
			}
		})
	}
}

// #5422: a linked worktree shares the mirror's config, so an agent's `git
// init` inside its run worktree rewrites the MIRROR as non-bare. The mirror's
// HEAD then counts as checked out and every later refresh fetch refuses to
// update it, failing every run in the gaggle until someone repairs the config.
// The manager owns the mirror's invariants and must restore them before it
// fetches.
func TestWorkingCopyRestoresMirrorBareAfterGitInitInRunWorktree(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	m := newTestManager(t)
	wt, err := m.Create(ctx, CreateOptions{RepoURL: repo, RunID: "run-init", BaseRef: "main"})
	if err != nil {
		t.Fatal(err)
	}
	dir, err := m.WorkingCopy(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}

	runTestGit(t, wt.Path, "init", "-q", ".")
	runTestGit(t, wt.Path, "config", "user.name", "Test User")
	if got := strings.TrimSpace(runTestGitAllowFailure(t, dir, "config", "--bool", "--get", "core.bare")); got != "false" {
		t.Fatalf("precondition: core.bare after git init in a linked worktree = %q, want false", got)
	}
	// Advance origin's checked-out branch so the refresh must update it.
	mustWriteFile(t, filepath.Join(repo, "next.txt"), "next\n")
	runTestGit(t, repo, "add", ".")
	runTestGit(t, repo, "commit", "-m", "next")

	if _, err := m.WorkingCopy(ctx, repo); err != nil {
		t.Fatalf("WorkingCopy after git init in a run worktree: %v — the mirror must be restored to bare before fetch (#5422)", err)
	}
	if got := strings.TrimSpace(runTestGit(t, dir, "config", "--bool", "--get", "core.bare")); got != "true" {
		t.Fatalf("core.bare after WorkingCopy = %q, want true", got)
	}
	want := strings.TrimSpace(runTestGit(t, repo, "rev-parse", "HEAD"))
	if got := strings.TrimSpace(runTestGit(t, dir, "rev-parse", "refs/heads/main")); got != want {
		t.Fatalf("mirror main = %s, want refreshed %s", got, want)
	}
}

// A mirror whose invariants already hold must not have its shared config
// rewritten on every refresh: that is a write a run worktree's own `git
// config` can contend with.
func TestEnsureMirrorInvariantsLeavesHealthyMirrorUntouched(t *testing.T) {
	ctx := context.Background()
	repo := newSourceRepo(t)
	m := newTestManager(t)
	dir, err := m.WorkingCopy(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "config")
	before, err := os.Stat(config)
	if err != nil {
		t.Fatal(err)
	}
	// Hold config.lock: any write would fail, so success proves none happened.
	mustWriteFile(t, config+".lock", "")
	if err := ensureMirrorInvariants(ctx, dir); err != nil {
		t.Fatalf("ensureMirrorInvariants on a healthy mirror: %v", err)
	}
	after, err := os.Stat(config)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("healthy mirror config rewritten")
	}
}
