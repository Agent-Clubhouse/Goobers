//go:build integration

package runner

import (
	"os"
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationParentContributionRetirementRequiresReadableArchive(t *testing.T) {
	testdep.Require(t, "git")
	source := t.TempDir()
	runGit(t, source, "init", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(source, "base.txt"), []byte("base"), 0600); err != nil {
		t.Fatal(err)
	}
	runGit(t, source, "add", ".")
	runGit(t, source, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "base")
	manager, err := worktree.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	wt, err := manager.Create(t.Context(), worktree.CreateOptions{RepoURL: source, RunID: "stage", OwnerRunID: "parent", BaseRef: "main", Branch: "runs/parent"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt.Path, "new.txt"), []byte("only copy of dirty contribution"), 0600); err != nil {
		t.Fatal(err)
	}
	custody, err := wt.HoldForChild(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	repo := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"}
	run, err := journal.Create(t.TempDir(), journal.RunIdentity{RunID: "parent", WorkspaceRepository: &repo}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })
	_, origin, err := run.AppendChildStageStarted(journal.Event{Type: journal.EventStageStarted, Stage: "work", Attempt: 1}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{Type: journal.EventRunnerAnnotation, Stage: "work", Attempt: 1, Runner: map[string]any{"kind": ContainedParentWorkspaceKind, "custody": ContainedParentWorkspaceCustody{Version: 1, Origin: origin, Workspace: custody}}}); err != nil {
		t.Fatal(err)
	}
	bytes := []byte("verified returned tree carrier")
	ref, err := run.RecordArtifact("returned-tree", bytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := RecordParentContribution(run, apiv1.InvocationEnvelope{RunID: "parent", ChildWorkflowOrigin: origin}, journal.Digest([]byte("contract")), ref); err != nil {
		t.Fatal(err)
	}
	r := &Runner{cfg: Config{Worktrees: manager, RepoCloneURL: func(apiv1.RepoRef) (string, error) { return source, nil }, ChildWorkflowAdmission: func(*workflow.Machine) error { return nil }}}
	for _, phase := range []journal.RunPhase{journal.PhaseFailed, journal.PhaseEscalated, journal.PhaseAborted} {
		if err := r.retireContainedParentContributions("parent", phase, run); err != nil {
			t.Fatal(err)
		}
		if _, err := manager.AdoptHeldStage(t.Context(), source, custody); err != nil {
			t.Fatal("unsuccessful terminal lost work", phase, err)
		}
	}
	if err := os.Remove(filepath.Join(run.Dir(), ref.Path)); err != nil {
		t.Fatal(err)
	}
	if err := r.retireContainedParentContributions("parent", journal.PhaseCompleted, run); err == nil {
		t.Fatal("missing archive authorized deletion")
	}
	if _, err := manager.AdoptHeldStage(t.Context(), source, custody); err != nil {
		t.Fatal("archive failure lost work", err)
	}
	if err := os.WriteFile(filepath.Join(run.Dir(), ref.Path), bytes, 0600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := r.retireContainedParentContributions("parent", journal.PhaseCompleted, run); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(wt.Path); !os.IsNotExist(err) {
		t.Fatal("verified completion checkout survived", err)
	}
	reader, _ := journal.OpenReadOnly(run.Dir())
	if _, err := reader.ArtifactBytesBounded(ref, 1024); err != nil {
		t.Fatal("final contribution evidence removed", err)
	}
}
