//go:build integration

package recovery

import (
	"bytes"
	"context"
	"errors"
	"github.com/goobers/goobers/test/testsupport/testdep"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestIntegrationChildDispositionPreparationAndRetry(t *testing.T) {
	testdep.Require(t, "git")
	repository, key, at := childSnapshotFixture(t)
	ctx := context.Background()
	fork := captureChildFixture(t, repository, key, "parent-fork", at, SnapshotPolicy{})
	child := filepath.Join(t.TempDir(), "child")
	recoveryTestGit(t, repository, "worktree", "add", "--detach", child, fork.Record.SnapshotSHA)
	childSnapshotWrite(t, child, "tracked.txt", "child edit\n")
	childSnapshotWrite(t, child, "child.bin", "\x00\xff")
	result := captureChildFixture(t, child, key, "child-result", at.Add(time.Second), SnapshotPolicy{})
	if result.Record.BaseSHA != fork.Record.SnapshotSHA {
		t.Fatal("child did not fork snapshot")
	}
	// Parent work after the fork is allowed only when named by a fresh expected
	// revision. It must survive a merge and be intentionally absent on replace.
	childSnapshotWrite(t, repository, "parent.txt", "parent edit\n")
	parent := captureChildFixture(t, repository, key, "parent-current", at.Add(2*time.Second), SnapshotPolicy{})
	// Ordinary recovery must retain its stricter main ancestry contract even
	// though child disposition can merge these verified sibling snapshots.
	if _, err := restoredSnapshotTree(ctx, repository, result.Record, parent.Record.SnapshotSHA, 1<<20); !errors.Is(err, ErrIncompatibleSnapshot) {
		t.Fatalf("ordinary recovery ancestry guard weakened: %v", err)
	}
	head := recoveryTestGit(t, repository, "rev-parse", "HEAD")
	beforeIndex, _ := os.ReadFile(filepath.Join(repository, ".git", "index"))
	for _, action := range []ChildDisposition{ChildMerge, ChildReplace, ChildDiscard} {
		t.Run(string(action), func(t *testing.T) {
			operation := "disposition-" + string(action)
			prepared, err := PrepareChildDisposition(ctx, repository, fork, parent, result.Record, action, operation, at.Add(3*time.Second), 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			retry, err := PrepareChildDisposition(ctx, repository, fork, parent, result.Record, action, operation, at.Add(3*time.Second), 1<<20)
			if err != nil || retry.Prepared.SnapshotSHA != prepared.Prepared.SnapshotSHA {
				t.Fatalf("retry=%+v %v", retry, err)
			}
			if err := VerifyChildDisposition(ctx, repository, prepared); err != nil {
				t.Fatal(err)
			}
			if got := recoveryTestGit(t, repository, "rev-parse", "HEAD"); got != head {
				t.Fatal("preparation moved parent HEAD")
			}
			afterIndex, _ := os.ReadFile(filepath.Join(repository, ".git", "index"))
			if !bytes.Equal(beforeIndex, afterIndex) {
				t.Fatal("preparation changed parent index")
			}
			if got, _ := os.ReadFile(filepath.Join(repository, "tracked.txt")); string(got) != "base\n" {
				t.Fatal("preparation changed live parent")
			}
			switch action {
			case ChildMerge:
				if got := recoveryTestGit(t, repository, "show", prepared.Prepared.SnapshotSHA+":parent.txt"); got != "parent edit" {
					t.Fatalf("merge lost parent: %q", got)
				}
				if got := recoveryTestGit(t, repository, "show", prepared.Prepared.SnapshotSHA+":tracked.txt"); got != "child edit" {
					t.Fatalf("merge lost child: %q", got)
				}
			case ChildReplace:
				if got := recoveryTestGit(t, repository, "ls-tree", "--name-only", prepared.Prepared.SnapshotSHA, "--", "parent.txt"); got != "" {
					t.Fatal("replace retained parent-only file")
				}
			case ChildDiscard:
				if prepared.TreeSHA != parent.TreeSHA {
					t.Fatal("discard changed working tree")
				}
			}
			altered := prepared
			altered.TreeSHA = fork.TreeSHA
			if prepared.TreeSHA != fork.TreeSHA {
				if err := VerifyChildDisposition(ctx, repository, altered); err == nil {
					t.Fatal("substituted tree accepted")
				}
			}
		})
	}
	// A changed action under one durable operation ID cannot overwrite its pin.
	if _, err := PrepareChildDisposition(ctx, repository, fork, parent, result.Record, ChildReplace, "disposition-merge", at.Add(3*time.Second), 1<<20); !errors.Is(err, ErrRecordConflict) {
		t.Fatalf("changed operation reused pin: %v", err)
	}
	childSnapshotWrite(t, repository, "parent.txt", "newer edit\n")
	if _, err := PrepareChildDisposition(ctx, repository, fork, parent, result.Record, ChildReplace, "stale", at.Add(4*time.Second), 1<<20); !errors.Is(err, ErrWorkspaceChanged) {
		t.Fatalf("moved parent accepted: %v", err)
	}
}

func TestIntegrationChildMergeConflictPreservesParent(t *testing.T) {
	testdep.Require(t, "git")
	repository, key, at := childSnapshotFixture(t)
	ctx := context.Background()
	fork := captureChildFixture(t, repository, key, "fork", at, SnapshotPolicy{})
	child := filepath.Join(t.TempDir(), "child")
	recoveryTestGit(t, repository, "worktree", "add", "--detach", child, fork.Record.SnapshotSHA)
	childSnapshotWrite(t, child, "tracked.txt", "child version\n")
	result := captureChildFixture(t, child, key, "result", at.Add(time.Second), SnapshotPolicy{})
	childSnapshotWrite(t, repository, "tracked.txt", "parent version\n")
	parent := captureChildFixture(t, repository, key, "expected", at.Add(2*time.Second), SnapshotPolicy{})
	if _, err := PrepareChildDisposition(ctx, repository, fork, parent, result.Record, ChildMerge, "conflict", at.Add(3*time.Second), 1<<20); err == nil {
		t.Fatal("conflicting three-way apply accepted")
	}
	if got, _ := os.ReadFile(filepath.Join(repository, "tracked.txt")); string(got) != "parent version\n" {
		t.Fatal("conflict partially applied to parent")
	}
	if err := CheckChildSnapshotCurrent(ctx, repository, parent); err != nil {
		t.Fatalf("conflict changed parent identity: %v", err)
	}
	if got := recoveryTestGit(t, repository, "for-each-ref", "--format=%(refname)", "refs/goobers/recovery/conflict"); got != "" {
		t.Fatal("conflict published prepared pin")
	}
}

func TestIntegrationChildDispositionMergeRefusesRewrittenParentHistory(t *testing.T) {
	testdep.Require(t, "git")
	repository, key, at := childSnapshotFixture(t)
	fork := captureChildFixture(t, repository, key, "fork", at, SnapshotPolicy{})
	child := filepath.Join(t.TempDir(), "child")
	recoveryTestGit(t, repository, "worktree", "add", "--detach", child, fork.Record.SnapshotSHA)
	childSnapshotWrite(t, child, "tracked.txt", "child edit\n")
	result := captureChildFixture(t, child, key, "child", at.Add(time.Second), SnapshotPolicy{})
	recoveryTestGit(t, repository, "checkout", "--orphan", "rewritten")
	recoveryTestGit(t, repository, "commit", "-m", "unrelated root")
	parent := captureChildFixture(t, repository, key, "rewritten-parent", at.Add(2*time.Second), SnapshotPolicy{})
	if _, err := PrepareChildDisposition(t.Context(), repository, fork, parent, result.Record, ChildMerge, "refused", at.Add(3*time.Second), 1<<20); !errors.Is(err, ErrIncompatibleSnapshot) {
		t.Fatalf("rewritten parent history was accepted: %v", err)
	}
}
