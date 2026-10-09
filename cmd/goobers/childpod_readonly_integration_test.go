//go:build integration

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

// Dispatch and interrupted-attempt recovery share this trusted stage selector.
// Neither may substitute the writable child or create a missing detached view.
func TestIntegrationChildPodReadOnlyCustodyUsesExactStage(t *testing.T) {
	testdep.Require(t, "git")
	source := t.TempDir()
	recoveryCLIGit(t, source, "init", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(source, "source.txt"), []byte("fork\n"), 0600); err != nil {
		t.Fatal(err)
	}
	recoveryCLIGit(t, source, "add", ".")
	recoveryCLIGit(t, source, "commit", "-m", "source")
	root := t.TempDir()
	manager, err := worktree.NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = manager.WorkingCopy(t.Context(), source); err != nil {
		t.Fatal(err)
	}
	manager, err = worktree.NewManager(root, worktree.WithRemoteGitGate(func(context.Context, string) error { return errors.New("recovery must not fetch") }))
	if err != nil {
		t.Fatal(err)
	}
	opts := worktree.ChildOptions{RepoURL: source, RunID: "child-workspace", OwnerRunID: "child-run", Gaggle: "web", SnapshotSHA: recoveryCLIGit(t, source, "rev-parse", "HEAD")}
	primary, err := manager.CreateChildFromSnapshot(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = adoptChildStageWorkspace(t.Context(), manager, opts, "inspect", apiv1.WorkspaceRepoReadOnly); err == nil {
		t.Fatal("missing view was provisioned by recovery")
	}
	view, err := manager.CreateChildReadOnlyView(t.Context(), opts, "inspect")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(primary.Path, "source.txt"), []byte("newer child edit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	adopted, err := adoptChildStageWorkspace(t.Context(), manager, opts, "inspect", apiv1.WorkspaceRepoReadOnly)
	if err != nil || adopted.Path != view.Path || adopted.Path == primary.Path {
		t.Fatal("read-only recovery selected wrong custody", adopted, err)
	}
	if data, err := os.ReadFile(filepath.Join(adopted.Path, "source.txt")); err != nil || string(data) != "fork\n" {
		t.Fatal("read-only view selected mutable child", string(data), err)
	}
	if _, err = adoptChildStageWorkspace(t.Context(), manager, opts, "other-stage", apiv1.WorkspaceRepoReadOnly); err == nil {
		t.Fatal("different stage adopted view")
	}
	writable, err := adoptChildStageWorkspace(t.Context(), manager, opts, "inspect", apiv1.WorkspaceRepo)
	if err != nil || writable.Path != primary.Path {
		t.Fatal("writable stage changed custody", writable, err)
	}
	if err = os.WriteFile(filepath.Join(view.Path, "source.txt"), []byte("unexpected edit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = adoptChildStageWorkspace(t.Context(), manager, opts, "inspect", apiv1.WorkspaceRepoReadOnly); err == nil {
		t.Fatal("recovery accepted changed read-only view")
	}
	if data, err := os.ReadFile(filepath.Join(view.Path, "source.txt")); err != nil || string(data) != "unexpected edit\n" {
		t.Fatal("recovery reset unexpected edit", string(data), err)
	}
}
