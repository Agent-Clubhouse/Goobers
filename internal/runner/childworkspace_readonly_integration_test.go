//go:build integration

package runner

import (
	"context"
	"errors"
	"os"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationChildReadOnlyStageUsesPinnedFork(t *testing.T) {
	testdep.Require(t, "git")
	for _, scenario := range []string{"direct", "human-resume", "changed-view"} {
		t.Run(scenario, func(t *testing.T) {
			f := prepareChildWorkspaceFixture(t, false)
			f.input.Machine = childWorkspaceMachine(t, apiv1.WorkspaceRepoReadOnly, scenario == "human-resume")
			childWorkspaceWrite(t, f.child.Path, "main.txt", []byte("later child edits\n"))
			manager, err := worktree.NewManager(f.config.Worktrees.Root, worktree.WithRemoteGitGate(func(context.Context, string) error { return errors.New("read-only fork must not fetch") }))
			if err != nil {
				t.Fatal(err)
			}
			f.config.Worktrees = manager
			var path string
			agent := func(env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
				path = env.Workspace
				if path == f.child.Path || path == f.parent {
					t.Fatal("read-only stage reused a writable workspace")
				}
				if head := gitOutput(t, path, "rev-parse", "HEAD"); head != f.input.ChildWorkspace.ForkSHA {
					t.Fatal("read-only view selected another revision", head)
				}
				childWorkspaceRead(t, path, "main.txt", []byte("parent dirty\n"))
				childWorkspaceRead(t, path, "parent.bin", []byte{0, 255, 1})
				if scenario == "changed-view" {
					childWorkspaceWrite(t, path, "main.txt", []byte("unexpected view edit\n"))
				}
				return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
			}
			result, err := f.runner(t, agent).Start(t.Context(), f.input)
			if scenario == "human-resume" {
				if err != nil || result.FinalState != "approval" || path != "" {
					t.Fatal("read-only child did not pause", result, err)
				}
				in := humanResumeInput(f.input.RunID, f.input.Machine, "approval", latestHumanPauseSeq(t, f.config.RunsDir, f.input.RunID, "approval"), "pass")
				in.GooberDigest = f.input.GooberDigest
				result, err = f.runner(t, agent).Resume(t.Context(), in)
			}
			if scenario == "changed-view" {
				if err == nil && result.Phase == journal.PhaseCompleted {
					t.Fatal("read-only mutation was accepted")
				}
				childWorkspaceRead(t, path, "main.txt", []byte("unexpected view edit\n"))
			} else {
				if err != nil || result.Phase != journal.PhaseCompleted {
					t.Fatal("read-only stage failed", result, err)
				}
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("clean read-only stage view was not removed", err)
				}
			}
			childWorkspaceRead(t, f.child.Path, "main.txt", []byte("later child edits\n"))
			f.assertParentUnchanged(t)
		})
	}
}

func TestIntegrationChildReadOnlyViewRefusesChangedCustody(t *testing.T) {
	testdep.Require(t, "git")
	for _, scenario := range []string{"missing", "changed-snapshot", "changed-head", "changed-file"} {
		t.Run(scenario, func(t *testing.T) {
			f := prepareChildWorkspaceFixture(t, false)
			r := &Runner{cfg: f.config}
			opts, err := r.childWorkspaceOptions(f.input)
			if err != nil {
				t.Fatal(err)
			}
			m := f.config.Worktrees
			if _, err := m.AdoptChildReadOnlyView(t.Context(), opts, "inspect"); err == nil {
				t.Fatal("adopt created a missing view")
			}
			view, err := m.CreateChildReadOnlyView(t.Context(), opts, "inspect")
			if err != nil {
				t.Fatal(err)
			}
			again, err := m.CreateChildReadOnlyView(t.Context(), opts, "inspect")
			if err != nil || again.Path != view.Path {
				t.Fatal("clean view replay failed", err)
			}
			switch scenario {
			case "missing":
				if err := os.Rename(view.Path, view.Path+"-removed"); err != nil {
					t.Fatal(err)
				}
			case "changed-snapshot":
				opts.SnapshotSHA = f.parentHead
			case "changed-head":
				runGit(t, view.Path, "checkout", "-b", "foreign-view")
			case "changed-file":
				childWorkspaceWrite(t, view.Path, "main.txt", []byte("retain this change\n"))
			}
			if _, err := m.AdoptChildReadOnlyView(t.Context(), opts, "inspect"); err == nil {
				t.Fatal("changed view custody was adopted")
			}
			if _, err := m.CreateChildReadOnlyView(t.Context(), opts, "inspect"); err == nil {
				t.Fatal("changed view custody was reset on retry")
			}
			if scenario == "changed-file" {
				childWorkspaceRead(t, view.Path, "main.txt", []byte("retain this change\n"))
			}
			if scenario == "missing" {
				if _, err := os.Stat(view.Path); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("missing view was recreated", err)
				}
			}
			f.assertParentUnchanged(t)
		})
	}
}

type childReadOnlyReviewer struct {
	review func(apiv1.InvocationEnvelope)
}

func (g childReadOnlyReviewer) Invoke(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	panic("unexpected task")
}
func (g childReadOnlyReviewer) Review(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	if done := invoke.RegisterWorkspaceWriter(ctx); done != nil {
		defer done(nil)
	}
	g.review(env)
	return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
}

func TestIntegrationChildReadOnlyReviewerRejectsMutations(t *testing.T) {
	testdep.Require(t, "git")
	for _, mutate := range []bool{false, true} {
		name := "clean"
		if mutate {
			name = "changed"
		}
		t.Run(name, func(t *testing.T) {
			f := prepareChildWorkspaceFixture(t, false)
			spec := apiv1.WorkflowSpec{Gaggle: "web", Start: "inspect", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
				Gates: []apiv1.Gate{{Name: "inspect", Evaluator: apiv1.EvaluatorAgentic,
					Agentic: &apiv1.AgenticGate{Goober: "reviewer", Workspace: apiv1.WorkspaceRepoReadOnly}, Branches: map[string]string{"pass": workflow.TerminalComplete, "needs-changes": workflow.TargetAbort, "fail": workflow.TargetAbort}}}}
			machine, err := workflow.Compile(workflow.Definition{Name: "generated-child", Version: 1, DSLVersion: "3.1", Spec: spec}, workflow.WithPreviewFeatures(true))
			if err != nil {
				t.Fatal(err)
			}
			f.input.Machine = machine
			var view string
			f.config.NewAgentic = func(string, ArtifactRecorder, SecretRegistrar) (invoke.Goober, error) {
				return childReadOnlyReviewer{review: func(env apiv1.InvocationEnvelope) {
					view = env.Workspace
					if view == f.child.Path || view == f.parent {
						t.Fatal("review reused writable custody")
					}
					childWorkspaceRead(t, view, "main.txt", []byte("parent dirty\n"))
					if mutate {
						childWorkspaceWrite(t, view, "main.txt", []byte("reviewer mutation\n"))
					}
				}}, nil
			}
			r, err := New(f.config)
			if err != nil {
				t.Fatal(err)
			}
			result, err := r.Start(t.Context(), f.input)
			if view == "" {
				t.Fatal("reviewer never ran", result, err)
			}
			if mutate {
				if err == nil && result.Phase == journal.PhaseCompleted {
					t.Fatal("mutating review accepted", result)
				}
				childWorkspaceRead(t, view, "main.txt", []byte("reviewer mutation\n"))
			} else if err != nil || result.Phase != journal.PhaseCompleted {
				t.Fatal(result, err)
			}
			f.assertParentUnchanged(t)
		})
	}
}
