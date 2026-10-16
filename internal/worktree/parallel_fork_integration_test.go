//go:build integration

package worktree_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationParallelForksIsolateVisitsAndPreserveRetryState(t *testing.T) {
	testdep.Require(t, "git")
	ctx := t.Context()
	source := t.TempDir()
	childWorkspaceGit(t, source, "init", "--initial-branch=main")
	write := func(dir, name, value string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(source, "seed.txt", "base\n")
	childWorkspaceGit(t, source, "add", ".")
	childWorkspaceGit(t, source, "commit", "-m", "base")
	base := childWorkspaceGit(t, source, "rev-parse", "HEAD")
	write(source, "seed.txt", "source\n")
	childWorkspaceGit(t, source, "commit", "-am", "source")
	snapshot := childWorkspaceGit(t, source, "rev-parse", "HEAD")
	root := t.TempDir()
	allowRemote := true
	newManager := func() *worktree.Manager {
		t.Helper()
		manager, err := worktree.NewManager(root, worktree.WithRemoteGitGate(func(context.Context, string) error {
			if !allowRemote {
				t.Fatal("isolated fork tried to acquire remote repository data")
			}
			return nil
		}))
		if err != nil {
			t.Fatal(err)
		}
		return manager
	}
	manager := newManager()
	if _, err := manager.WorkingCopy(ctx, source); err != nil {
		t.Fatal(err)
	}
	allowRemote = false
	opts := worktree.ParallelForkOptions{RepoURL: source, OwnerRunID: "parent", Gaggle: "web", ParallelSequence: 17, Branch: 1, SnapshotSHA: snapshot}
	first, err := manager.CreateParallelFromSnapshot(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	siblingOpts := opts
	siblingOpts.Branch = 2
	sibling, err := manager.CreateParallelFromSnapshot(ctx, siblingOpts)
	if err != nil {
		t.Fatal(err)
	}
	if first.Path == sibling.Path || first.Branch == sibling.Branch || first.RunID == sibling.RunID {
		t.Fatal("parallel siblings share physical custody")
	}
	write(first.Path, "committed.txt", "branch commit\n")
	childWorkspaceGit(t, first.Path, "add", "committed.txt")
	childWorkspaceGit(t, first.Path, "commit", "-m", "branch progression")
	write(first.Path, "seed.txt", "staged\n")
	childWorkspaceGit(t, first.Path, "add", "seed.txt")
	write(first.Path, "seed.txt", "working\n")
	write(first.Path, "untracked.bin", "untracked\x00bytes")
	head := childWorkspaceGit(t, first.Path, "rev-parse", "HEAD")
	index := childWorkspaceGit(t, first.Path, "write-tree")
	assertFirst := func() {
		t.Helper()
		if childWorkspaceGit(t, first.Path, "rev-parse", "HEAD") != head || childWorkspaceGit(t, first.Path, "write-tree") != index {
			t.Fatal("retry reset committed or staged work")
		}
		for name, want := range map[string]string{"seed.txt": "working\n", "untracked.bin": "untracked\x00bytes"} {
			data, err := os.ReadFile(filepath.Join(first.Path, name))
			if err != nil || string(data) != want {
				t.Fatal("retry changed working state", name, err)
			}
		}
	}
	manager = newManager()
	for _, adopt := range []bool{false, true} {
		var retried *worktree.Worktree
		if adopt {
			retried, err = manager.AdoptParallelFromSnapshot(ctx, opts)
		} else {
			retried, err = manager.CreateParallelFromSnapshot(ctx, opts)
		}
		if err != nil || retried.Path != first.Path || retried.Branch != first.Branch {
			t.Fatal("restart lost exact fork custody", err)
		}
		assertFirst()
	}
	changed := opts
	changed.SnapshotSHA = base
	if _, err := manager.CreateParallelFromSnapshot(ctx, changed); err == nil {
		t.Fatal("changed source created replacement custody for the same visit")
	}
	if _, err := manager.AdoptParallelFromSnapshot(ctx, changed); err == nil {
		t.Fatal("recovery accepted a different source")
	}
	assertFirst()
	later := opts
	later.ParallelSequence++
	if _, err := manager.AdoptParallelFromSnapshot(ctx, later); err == nil {
		t.Fatal("missing later visit adopted earlier workspace")
	}
	next, err := manager.CreateParallelFromSnapshot(ctx, later)
	if err != nil || next.Path == first.Path || next.Branch == first.Branch {
		t.Fatal("later visit reused earlier physical workspace", err)
	}
	for _, dir := range []string{source, sibling.Path, next.Path} {
		data, err := os.ReadFile(filepath.Join(dir, "seed.txt"))
		if err != nil || string(data) != "source\n" || childWorkspaceGit(t, dir, "rev-parse", "HEAD") != snapshot {
			t.Fatal("first branch changed source, sibling, or later visit", dir, err)
		}
		if _, err := os.Stat(filepath.Join(dir, "untracked.bin")); !os.IsNotExist(err) {
			t.Fatal("untracked changes leaked to another workspace", dir, err)
		}
	}
	custody, err := first.HoldForChild(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.CreateParallelFromSnapshot(ctx, opts); err == nil {
		t.Fatal("generic retry bypassed child custody hold")
	}
	if _, err := manager.AdoptParallelFromSnapshot(ctx, opts); err == nil {
		t.Fatal("snapshot selector bypassed child custody hold")
	}
	adopted, err := manager.AdoptHeldStage(ctx, source, custody)
	if err != nil || adopted.Path != first.Path {
		t.Fatal("exact held-stage custody could not recover branch", err)
	}
	assertFirst()
}
