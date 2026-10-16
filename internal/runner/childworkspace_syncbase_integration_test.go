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
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

type childSyncBaseExecutor struct {
	run func(apiv1.InvocationEnvelope)
}

func (e childSyncBaseExecutor) Run(ctx context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	if done := invoke.RegisterWorkspaceWriter(ctx); done != nil {
		defer done(nil)
	}
	e.run(env)
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}

func TestIntegrationChildSyncBasePreservesForkAndParent(t *testing.T) {
	testdep.Require(t, "git")
	for _, scenario := range []string{"direct", "human-resume", "dirty", "conflict", "no-sync", "fetch-denied"} {
		t.Run(scenario, func(t *testing.T) {
			f := prepareChildWorkspaceFixture(t, false)
			sync := scenario != "no-sync"
			spec := apiv1.WorkflowSpec{Gaggle: "web", Start: "build", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerManual}},
				Tasks: []apiv1.Task{{Name: "build", Type: apiv1.TaskDeterministic, Goal: "build from synchronized child", Run: &apiv1.DeterministicRun{Command: []string{"true"}, SyncBase: sync}, Workspace: apiv1.WorkspaceRepo}}}
			if scenario == "human-resume" {
				spec.Start = "approval"
				spec.Gates = []apiv1.Gate{{Name: "approval", Evaluator: apiv1.EvaluatorHuman, Human: &apiv1.HumanGate{}, Branches: map[string]string{"pass": "build", "reject": workflow.TargetAbort}}}
			}
			machine, err := workflow.Compile(workflow.Definition{Name: "generated-child", Version: 1, DSLVersion: "3.1", Spec: spec}, workflow.WithPreviewFeatures(true))
			if err != nil {
				t.Fatal(err)
			}
			f.input.Machine = machine
			childWorkspaceWrite(t, f.parent, "upstream.txt", []byte("new upstream\n"))
			runGit(t, f.parent, "add", "upstream.txt")
			if scenario == "conflict" {
				childWorkspaceWrite(t, f.parent, "main.txt", []byte("conflicting upstream\n"))
				runGit(t, f.parent, "add", "main.txt")
			}
			runGit(t, f.parent, "commit", "-m", "advance configured base")
			f.parentHead = gitOutput(t, f.parent, "rev-parse", "HEAD")
			f.parentIndex, err = os.ReadFile(filepath.Join(f.parent, ".git", "index"))
			if err != nil {
				t.Fatal(err)
			}
			childWorkspaceWrite(t, f.parent, "main.txt", []byte("parent dirty\n"))
			if scenario == "dirty" {
				childWorkspaceWrite(t, f.child.Path, "main.txt", []byte("retain unfinished work\n"))
			}
			fetches := 0
			f.config.Worktrees, err = worktree.NewManager(f.config.Worktrees.Root, worktree.WithRemoteGitGate(func(context.Context, string) error {
				fetches++
				if scenario == "fetch-denied" {
					return errors.New("configured remote operation denied")
				}
				return nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			f.config.NewDeterministic = func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
				return childSyncBaseExecutor{run: func(env apiv1.InvocationEnvelope) {
					calls++
					if env.Workspace != f.child.Path {
						t.Fatal("sync changed child custody")
					}
					childWorkspaceRead(t, env.Workspace, "main.txt", []byte("parent dirty\n"))
					if sync {
						childWorkspaceRead(t, env.Workspace, "upstream.txt", []byte("new upstream\n"))
					} else if _, err := os.Stat(filepath.Join(env.Workspace, "upstream.txt")); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("undeclared base sync", err)
					}
					runGit(t, env.Workspace, "merge-base", "--is-ancestor", f.input.ChildWorkspace.ForkSHA, "HEAD")
				}}, nil
			}
			r, err := New(f.config)
			if err != nil {
				t.Fatal(err)
			}
			result, err := r.Start(t.Context(), f.input)
			if scenario == "human-resume" {
				if err != nil || result.FinalState != "approval" || calls != 0 || fetches != 0 {
					t.Fatal(result, err, calls, fetches)
				}
				r, err = New(f.config)
				if err != nil {
					t.Fatal(err)
				}
				in := humanResumeInput(f.input.RunID, machine, "approval", latestHumanPauseSeq(t, f.config.RunsDir, f.input.RunID, "approval"), "pass")
				in.GooberDigest = f.input.GooberDigest
				result, err = r.Resume(t.Context(), in)
			}
			refused := scenario == "dirty" || scenario == "conflict" || scenario == "fetch-denied"
			if refused {
				if (err == nil && result.Phase == journal.PhaseCompleted) || calls != 0 {
					t.Fatal("unsafe base sync dispatched", result, err, calls)
				}
				if head := gitOutput(t, f.child.Path, "rev-parse", "HEAD"); head != f.input.ChildWorkspace.ForkSHA {
					t.Fatal("failed sync changed child HEAD", head)
				}
				if scenario == "dirty" {
					childWorkspaceRead(t, f.child.Path, "main.txt", []byte("retain unfinished work\n"))
				} else if status := gitOutput(t, f.child.Path, "status", "--porcelain"); status != "" {
					t.Fatal("failed sync left merge debris", status)
				}
			} else if err != nil || result.Phase != journal.PhaseCompleted || calls != 1 {
				t.Fatal(result, err, calls)
			}
			if (scenario == "no-sync" || scenario == "dirty") && fetches != 0 {
				t.Fatal("unnecessary remote acquisition", fetches)
			}
			if sync && scenario != "dirty" && fetches == 0 {
				t.Fatal("base sync bypassed remote gate")
			}
			f.assertParentUnchanged(t)
		})
	}
}
