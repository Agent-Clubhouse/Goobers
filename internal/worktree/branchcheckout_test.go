package worktree

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestWithExistingBranchCheckout(t *testing.T) {
	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := newSourceRepo(t)
	const branch = "goobers/implementation/terminal-run"
	workspace, err := manager.Create(t.Context(), CreateOptions{
		RepoURL: repo, RunID: "terminal-run", OwnerRunID: "terminal-run",
		BaseRef: "main", Branch: branch,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace.Path, "retained.txt"), []byte("terminal state"), 0o600); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, workspace.Path, "add", "retained.txt")
	runTestGit(t, workspace.Path, "commit", "-m", "retain terminal state")
	if err := workspace.Remove(t.Context(), RemoveOptions{}); err != nil {
		t.Fatal(err)
	}

	var checkout string
	found, err := manager.WithExistingBranchCheckout(t.Context(), repo, branch, "main", func(path string) error {
		checkout = path
		data, err := os.ReadFile(filepath.Join(path, "retained.txt"))
		if err != nil {
			return err
		}
		if string(data) != "terminal state" {
			t.Fatalf("checkout content = %q", data)
		}
		runTestGit(t, path, "update-ref", "refs/goobers/recovery/test", "HEAD")
		return nil
	})
	if err != nil || !found {
		t.Fatalf("checkout = %t, %v", found, err)
	}
	if _, err := os.Stat(checkout); !os.IsNotExist(err) {
		t.Fatalf("temporary checkout remained: %v", err)
	}
	if _, err := gitOutput(t.Context(), manager.repoDirForKey(repoKey(repo)), "rev-parse", "--verify", "refs/goobers/recovery/test"); err != nil {
		t.Fatalf("recovery ref did not persist in managed mirror: %v", err)
	}
}

func TestWithExistingBranchCheckoutDoesNotCreateMirror(t *testing.T) {
	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const repo = "https://example.invalid/owner/repo.git"
	found, err := manager.WithExistingBranchCheckout(context.Background(), repo, "missing", "main", func(string) error {
		t.Fatal("unexpected checkout")
		return nil
	})
	if err != nil || found {
		t.Fatalf("checkout = %t, %v", found, err)
	}
	if _, err := os.Stat(filepath.Join(manager.Root, repoKey(repo))); !os.IsNotExist(err) {
		t.Fatalf("lookup created repository state: %v", err)
	}
}
