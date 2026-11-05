package workerhost

import (
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/worktree"
)

func TestWorkspaceCommitInspectorUsesAttemptBaseline(t *testing.T) {
	repo := newFixtureRepo(t)
	manager, err := worktree.NewManager(filepath.Join(t.TempDir(), "workcopies"))
	if err != nil {
		t.Fatal(err)
	}
	provisioner := &WorktreeWorkspaces{Manager: manager, CloneURL: func(apiv1.RepoRef) (string, error) { return repo, nil }}
	request := engine.WorkspaceRequest{RunID: "commit-observation", Stage: "work", Gaggle: "web", Workflow: "implementation", Mode: apiv1.WorkspaceRepo, RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"}}
	for attempt := 0; attempt < 2; attempt++ {
		ws, err := provisioner.Provision(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		inspector, ok := ws.(engine.CommitInspector)
		if !ok {
			t.Fatal("production workspace lacks commit observation")
		}
		if got, err := inspector.HasNewCommits(t.Context()); err != nil || got {
			t.Fatalf("prior attempt counted: committed=%v err=%v", got, err)
		}
		runGit(t, ws.Path(), "config", "user.name", "test")
		runGit(t, ws.Path(), "config", "user.email", "test@example.com")
		runGit(t, ws.Path(), "commit", "--allow-empty", "-m", "attempt commit")
		if got, err := inspector.HasNewCommits(t.Context()); err != nil || !got {
			t.Fatalf("new commit absent: committed=%v err=%v", got, err)
		}
		if err := ws.Remove(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
}
