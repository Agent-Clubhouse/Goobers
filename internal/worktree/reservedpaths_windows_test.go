package worktree

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveCleansWindowsReservedPathBeforeRepass(t *testing.T) {
	ctx := context.Background()
	repository := newSourceRepo(t)
	manager := newTestManager(t)
	if err := manager.SetCleanupGuard("recovery", func(context.Context, CleanupTarget) error {
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	create := CreateOptions{
		RepoURL:    repository,
		RunID:      "reserved-stage",
		OwnerRunID: "reserved-run",
		BaseRef:    "main",
		Branch:     "goobers/test/reserved",
	}
	worktree, err := manager.Create(ctx, create)
	if err != nil {
		t.Fatal(err)
	}
	reserved := `\\?\` + filepath.Join(worktree.Path, "test", "ci", "NUL")
	if err := os.MkdirAll(filepath.Dir(reserved), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reserved, []byte("local CI residue"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := worktree.Remove(ctx, RemoveOptions{}); err != nil {
		t.Fatalf("remove worktree containing reserved path: %v", err)
	}

	create.RunID = "reserved-stage-repass"
	create.Branch = "goobers/test/reserved-repass"
	repass, err := manager.Create(ctx, create)
	if err != nil {
		t.Fatalf("create repass worktree: %v", err)
	}
	if err := repass.Remove(ctx, RemoveOptions{}); err != nil {
		t.Fatalf("remove repass worktree: %v", err)
	}
}

func TestRemoveWindowsReservedCleanupPathsSkipsSettledTargets(t *testing.T) {
	ctx := context.Background()
	missing := filepath.Join(t.TempDir(), "gone")
	if err := removeWindowsReservedCleanupPaths(ctx, missing); err != nil {
		t.Fatalf("missing target: %v", err)
	}
	if err := removeWindowsReservedCleanupPaths(ctx, t.TempDir()); err != nil {
		t.Fatalf("repository-less target: %v", err)
	}
	dangling := t.TempDir()
	gitFile := "gitdir: " + filepath.ToSlash(filepath.Join(t.TempDir(), "missing-admin", "wt1")) + "\n"
	if err := os.WriteFile(filepath.Join(dangling, ".git"), []byte(gitFile), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeWindowsReservedCleanupPaths(ctx, dangling); err != nil {
		t.Fatalf("dangling gitfile target: %v", err)
	}
}
