//go:build integration

package recovery

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func applicationFixture(t *testing.T, action ChildDisposition) (string, ChildApplyPlan) {
	t.Helper()
	repo, key, at := childSnapshotFixture(t)
	policy := SnapshotPolicy{ExcludedPaths: []string{"auth"}}
	recoveryTestGit(t, repo, "update-index", "--assume-unchanged", ".gitignore")
	childSnapshotWrite(t, repo, "auth/token", "excluded value")
	recoveryTestGit(t, repo, "add", "-f", "auth/token")
	childSnapshotWrite(t, repo, "tracked.txt", "staged parent content\n")
	recoveryTestGit(t, repo, "add", "tracked.txt")
	childSnapshotWrite(t, repo, "tracked.txt", "dirty parent content\n")
	fork := captureChildFixture(t, repo, key, "fork", at, policy)
	child := filepath.Join(t.TempDir(), "child")
	recoveryTestGit(t, repo, "worktree", "add", "--detach", child, fork.Record.SnapshotSHA)
	childSnapshotWrite(t, child, "tracked.txt", "child content\n")
	childSnapshotWrite(t, child, "nested/child.bin", "\x00\xff\x01")
	if err := os.Chmod(filepath.Join(child, "nested/child.bin"), 0755); err != nil {
		t.Fatal(err)
	}
	childSnapshotWrite(t, child, "ignored.out", "tracked ignored child file")
	recoveryTestGit(t, child, "add", "--force", "ignored.out")
	if err := os.Remove(filepath.Join(child, "deleted.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("tracked.txt", filepath.Join(child, "safe-link")); err != nil {
		t.Fatal(err)
	}
	result := captureChildFixture(t, child, key, "result", at.Add(time.Second), policy)
	childSnapshotWrite(t, repo, "parent-only.txt", "parent addition")
	parent := captureChildFixture(t, repo, key, "expected-parent", at.Add(2*time.Second), policy)
	prepared, err := PrepareChildDisposition(t.Context(), repo, fork, parent, result.Record, action, "apply-"+string(action), at.Add(3*time.Second), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := PlanChildApplication(t.Context(), repo, prepared)
	if err != nil {
		t.Fatal(err)
	}
	return repo, plan
}

func TestIntegrationChildApplicationActionsPreserveProtectedState(t *testing.T) {
	for _, action := range []ChildDisposition{ChildMerge, ChildReplace, ChildDiscard} {
		t.Run(string(action), func(t *testing.T) {
			repo, plan := applicationFixture(t, action)
			head := recoveryTestGit(t, repo, "rev-parse", "HEAD")
			excluded := recoveryTestGit(t, repo, "ls-files", "--stage", "auth/token")
			if err := ApplyChildApplication(t.Context(), repo, plan); err != nil {
				t.Fatal(err)
			}
			if err := ApplyChildApplication(t.Context(), repo, plan); err != nil {
				t.Fatalf("retry: %v", err)
			}
			if err := VerifyChildApplication(t.Context(), repo, plan); err != nil {
				t.Fatal(err)
			}
			if recoveryTestGit(t, repo, "rev-parse", "HEAD") != head || recoveryTestGit(t, repo, "ls-files", "--stage", "auth/token") != excluded {
				t.Fatal("changed parent HEAD or excluded index entry")
			}
			if data, _ := os.ReadFile(filepath.Join(repo, "auth/token")); string(data) != "excluded value" {
				t.Fatal("lost excluded file")
			}
			if action == ChildDiscard {
				if _, err := os.Stat(filepath.Join(repo, "ignored.out")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("discard applied child output")
				}
				return
			}
			if data, _ := os.ReadFile(filepath.Join(repo, "ignored.out")); string(data) != "tracked ignored child file" {
				t.Fatal("lost tracked ignored child output")
			}
			if recoveryTestGit(t, repo, "ls-files", "ignored.out") != "ignored.out" {
				t.Fatal("ignored child addition must remain tracked in parent")
			}
			if data, _ := os.ReadFile(filepath.Join(repo, "nested/child.bin")); string(data) != "\x00\xff\x01" {
				t.Fatal("binary output changed")
			}
			if info, err := os.Stat(filepath.Join(repo, "nested/child.bin")); err != nil || info.Mode().Perm()&0111 == 0 {
				t.Fatal("child executable mode lost")
			}
			if flags := recoveryTestGit(t, repo, "ls-files", "-v", ".gitignore"); flags != "h .gitignore" {
				t.Fatal("unrelated index flags changed", flags)
			}
			if link, err := os.Readlink(filepath.Join(repo, "safe-link")); err != nil || link != "tracked.txt" {
				t.Fatal("safe symlink was not preserved")
			}
			_, err := os.Stat(filepath.Join(repo, "parent-only.txt"))
			if action == ChildMerge && err != nil {
				t.Fatal("merge lost parent addition")
			}
			if action == ChildReplace && !errors.Is(err, os.ErrNotExist) {
				t.Fatal("replace retained parent addition")
			}
		})
	}
}

func TestIntegrationChildApplicationRecoversPartialFilesAndIndex(t *testing.T) {
	repo, plan := applicationFixture(t, ChildMerge)
	changes, err := loadChildChanges(t.Context(), repo, plan.Disposition)
	if err != nil || len(changes) < 2 {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(repo)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := writeChildFile(root, changes[0], plan.Disposition.Prepared.SnapshotSHA); err != nil {
		t.Fatal(err)
	}
	if err := ApplyChildApplication(t.Context(), repo, plan); err != nil {
		t.Fatalf("recover partial files: %v", err)
	}
	if err := ApplyChildApplication(t.Context(), repo, plan); err != nil {
		t.Fatalf("recover applied index before ack: %v", err)
	}
}

func TestIntegrationChildApplicationRefusesInterveningChanges(t *testing.T) {
	for _, kind := range []string{"changed-path", "unrelated-path", "index", "head", "symlink-ancestor"} {
		t.Run(kind, func(t *testing.T) {
			repo, plan := applicationFixture(t, ChildMerge)
			switch kind {
			case "changed-path":
				childSnapshotWrite(t, repo, "tracked.txt", "intervening work")
			case "unrelated-path":
				childSnapshotWrite(t, repo, "parent-only.txt", "intervening work")
			case "index":
				recoveryTestGit(t, repo, "add", "parent-only.txt")
			case "head":
				recoveryTestGit(t, repo, "commit", "-m", "new parent commit")
			case "symlink-ancestor":
				if err := os.Symlink("auth", filepath.Join(repo, "nested")); err != nil {
					t.Fatal(err)
				}
			}
			if err := ApplyChildApplication(t.Context(), repo, plan); err == nil {
				t.Fatal("intervening change accepted")
			}
			if data, _ := os.ReadFile(filepath.Join(repo, "auth/token")); string(data) != "excluded value" {
				t.Fatal("touched excluded value")
			}
			if _, err := os.Lstat(filepath.Join(repo, "safe-link")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("partially applied despite failed preflight")
			}
		})
	}
}
