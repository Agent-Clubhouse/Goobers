//go:build integration

package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationParentContributionSurvivesOrdinaryStages(t *testing.T) {
	testdep.Require(t, "git")
	r, run, frame, _, _ := prepareChildWaitRuntime(t)
	if err := frame.recordTaskStarted(1, ""); err != nil {
		t.Fatal(err)
	}
	workspace, err := r.createStageWorkspace(t.Context(), frame.in, frame.t.Name, apiv1.WorkspaceRepo, false, "")
	if err != nil {
		t.Fatal(err)
	}
	childWorkspaceWrite(t, workspace.path, "main.txt", []byte("staged parent\n"))
	runGit(t, workspace.path, "add", "main.txt")
	childWorkspaceWrite(t, workspace.path, "main.txt", []byte("dirty parent\n"))
	originalHead := gitOutput(t, workspace.path, "rev-parse", "HEAD")
	env := apiv1.InvocationEnvelope{RunID: frame.in.RunID, Gaggle: frame.in.Gaggle, WorkflowID: frame.in.Machine.Def.Name, Attempt: 1, ChildWorkflowOrigin: frame.childOrigin}
	if err := holdContainedParentWorkspace(t.Context(), frame, workspace, env); err != nil {
		t.Fatal(err)
	}
	// The transport owns validation/import; the runner consumes its durable ref.
	output, err := run.RecordArtifact("verified-parent-output.json", []byte("verified transport output"))
	if err != nil {
		t.Fatal(err)
	}
	if err := RecordParentContribution(run, env, journal.Digest([]byte("contract")), output); err != nil {
		t.Fatal(err)
	}
	if err := run.Append(journal.Event{Type: journal.EventStageFinished, Stage: frame.t.Name, Attempt: 1, Status: string(apiv1.ResultSuccess)}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	r.cfg.NewAgentic = func(string, ArtifactRecorder, SecretRegistrar) (invoke.Goober, error) {
		return childWorkspaceAgent{invoke: func(env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
			calls++
			if env.Workspace != workspace.path || env.ChildWorkflowOrigin != nil {
				return apiv1.ResultEnvelope{}, errors.New("ordinary stage did not inherit exact parent checkout")
			}
			childWorkspaceRead(t, env.Workspace, "main.txt", []byte("dirty parent\n"))
			if got := gitOutput(t, env.Workspace, "show", ":main.txt"); got != "staged parent" {
				return apiv1.ResultEnvelope{}, errors.New("ordinary stage lost staged parent edit")
			}
			if calls == 1 {
				childWorkspaceWrite(t, env.Workspace, "ordinary.txt", []byte("ordinary commit\n"))
				runGit(t, env.Workspace, "config", "user.name", "Continuity Test")
				runGit(t, env.Workspace, "config", "user.email", "continuity@example.invalid")
				runGit(t, env.Workspace, "add", "ordinary.txt")
				runGit(t, env.Workspace, "commit", "--only", "-m", "ordinary stage commit", "ordinary.txt")
			} else {
				childWorkspaceRead(t, env.Workspace, "ordinary.txt", []byte("ordinary commit\n"))
				if gitOutput(t, env.Workspace, "rev-parse", "HEAD") == originalHead {
					return apiv1.ResultEnvelope{}, errors.New("subsequent stage lost ordinary commit")
				}
			}
			return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
		}}, nil
	}
	frame.ex = childOriginExecutors(r.cfg, run)
	frame.t.ChildWorkflows = nil
	frame.childOrigin = nil
	for _, name := range []string{"ordinary", "following"} {
		frame.t.Name = name
		result, _, err := r.runTask(t.Context(), frame, 0, 1, "", "", nil, false, nil)
		if err != nil || result.Status != apiv1.ResultSuccess {
			t.Fatal("ordinary continuation failed", result, err)
		}
	}
	if calls != 2 {
		t.Fatal("ordinary stages did not execute", calls)
	}
	for _, mode := range []apiv1.WorkspaceMode{apiv1.WorkspaceRepo, apiv1.WorkspaceRepoReadOnly} {
		g := apiv1.Gate{Name: "review-" + string(mode), Evaluator: apiv1.EvaluatorAgentic, Agentic: &apiv1.AgenticGate{Goober: "reviewer", Workspace: mode}}
		env, review, _, err := r.buildGateEnvelopeWithRetry(t.Context(), run, frame.in, g, nil, apiv1.Limits{}, nil, "")
		if err != nil {
			t.Fatal("review workspace", mode, err)
		}
		if mode == apiv1.WorkspaceRepo {
			if env.Workspace != workspace.path {
				t.Fatal("review lost retained checkout")
			}
			childWorkspaceRead(t, env.Workspace, "main.txt", []byte("dirty parent\n"))
		} else {
			if env.Workspace == workspace.path {
				t.Fatal("read-only review borrowed writable parent")
			}
			childWorkspaceRead(t, env.Workspace, "main.txt", []byte("base\n"))
		}
		if err := review.Remove(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(workspace.path, "ordinary.txt")); err != nil {
		t.Fatal("stage cleanup removed contribution", err)
	}
	if _, err := r.cfg.Worktrees.FinalizeRun(context.Background(), frame.in.RunID); err != nil {
		t.Fatal(err)
	}
	childWorkspaceRead(t, workspace.path, "main.txt", []byte("dirty parent\n"))
}

func TestIntegrationHeldParentBaseSyncPreservesEditsAndStageBaseline(t *testing.T) {
	testdep.Require(t, "git")
	r, _, frame, _, source := prepareChildWaitRuntime(t)
	workspace, err := r.createStageWorkspace(t.Context(), frame.in, frame.t.Name, apiv1.WorkspaceRepo, false, "")
	if err != nil {
		t.Fatal(err)
	}
	custody, err := workspace.worktree.HoldForChild(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	childWorkspaceWrite(t, workspace.path, "main.txt", []byte("retained edit\n"))
	runGit(t, workspace.path, "add", "main.txt")
	before := gitOutput(t, workspace.path, "write-tree")
	if err := workspace.worktree.PrepareHeldStage(t.Context(), "main", true); err == nil {
		t.Fatal("base synchronization accepted uncommitted edits")
	}
	childWorkspaceRead(t, workspace.path, "main.txt", []byte("retained edit\n"))
	if gitOutput(t, workspace.path, "write-tree") != before {
		t.Fatal("base synchronization rewrote staged edits")
	}
	runGit(t, workspace.path, "commit", "-m", "retain parent work")
	childWorkspaceWrite(t, source.parent, "incoming.txt", []byte("new base\n"))
	runGit(t, source.parent, "add", "incoming.txt")
	runGit(t, source.parent, "commit", "--only", "-m", "advance base", "incoming.txt")
	if err := workspace.worktree.PrepareHeldStage(t.Context(), "main", true); err != nil {
		t.Fatal(err)
	}
	childWorkspaceRead(t, workspace.path, "incoming.txt", []byte("new base\n"))
	childWorkspaceRead(t, workspace.path, "main.txt", []byte("retained edit\n"))
	if err := workspace.worktree.PrepareHeldStage(t.Context(), "main", false); err != nil {
		t.Fatal(err)
	}
	changed, err := workspace.worktree.HasNewCommits(t.Context())
	if err != nil || changed {
		t.Fatal("earlier commits counted as current stage work", changed, err)
	}
	if _, err := r.cfg.Worktrees.AdoptHeldStage(t.Context(), source.parent, custody); err != nil {
		t.Fatal("base sync replaced immutable custody", err)
	}
	head := gitOutput(t, workspace.path, "rev-parse", "HEAD")
	runGit(t, source.parent, "add", "main.txt")
	runGit(t, source.parent, "commit", "--only", "-m", "conflicting base", "main.txt")
	if err := workspace.worktree.PrepareHeldStage(t.Context(), "main", true); err == nil {
		t.Fatal("conflicting base unexpectedly merged")
	}
	if gitOutput(t, workspace.path, "rev-parse", "HEAD") != head || gitOutput(t, workspace.path, "status", "--porcelain") != "" {
		t.Fatal("failed synchronization left partial merge")
	}
	childWorkspaceRead(t, workspace.path, "main.txt", []byte("retained edit\n"))
	if _, err := r.cfg.Worktrees.AdoptHeldStage(t.Context(), source.parent, custody); err != nil {
		t.Fatal("failed sync lost custody", err)
	}
}
