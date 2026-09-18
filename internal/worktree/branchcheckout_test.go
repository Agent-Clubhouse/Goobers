package worktree

import (
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
		return nil
	})
	if err != nil || !found {
		t.Fatalf("checkout = %t, %v", found, err)
	}
	if _, err := os.Stat(checkout); !os.IsNotExist(err) {
		t.Fatalf("temporary checkout remained: %v", err)
	}
}
