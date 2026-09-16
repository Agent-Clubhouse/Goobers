package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/workspacebranch"
	"github.com/goobers/goobers/internal/worktree"
)

func TestOwnedBranchLocalContinuityAndResume(t *testing.T) {
	for _, pinned := range []bool{false, true} {
		for _, resume := range []bool{false, true} {
			name := "disposable"
			if pinned {
				name = "pinned"
			}
			if resume {
				name += "-resume"
			}
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				repo := newRebindFixtureRepo(t)
				sha := strings.TrimSpace(gitOutput(t, "", "--git-dir="+repo, "rev-parse", rebindBranch))
				base := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"}
				selection := revisionFixture(sha)
				tasks := []apiv1.Task{
					{Name: "select", Type: apiv1.TaskDeterministic, Goal: "select", Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}, Next: "establish"},
					{Name: "establish", Type: apiv1.TaskDeterministic, Goal: "establish", Inputs: map[string]string{"kind": workspacebranch.KindEstablish}, Capabilities: []string{"repo:push"},
						Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}, Next: "author"},
					{Name: "author", Type: apiv1.TaskDeterministic, Goal: "author", Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceRepo}, Next: "publish"},
					{Name: "publish", Type: apiv1.TaskDeterministic, Goal: "publish", Inputs: map[string]string{"kind": workspacebranch.KindPublish}, Capabilities: []string{"repo:push"},
						Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceRepo}, Next: workflow.TerminalComplete},
				}
				machine, err := workflow.Compile(workflow.Definition{Name: "owned", Version: 1, Spec: apiv1.WorkflowSpec{
					Gaggle: "web", Start: "select", Tasks: tasks,
					Triggers: []apiv1.Trigger{{Type: apiv1.TriggerSchedule, Schedule: "0 * * * *"}},
				}})
				if err != nil {
					t.Fatal(err)
				}
				const id = "owned-run"
				binding, err := workspacebranch.Expected(base, selection, "", machine.Def.Name, id)
				if err != nil {
					t.Fatal(err)
				}
				opts := worktree.RemoteBranchOptions{SourceURL: repo, TargetURL: repo, Binding: *binding,
					SourceRead: worktree.RemoteBranchAccess{Authorized: true}, TargetWrite: worktree.RemoteBranchAccess{Authorized: true}}
				root := t.TempDir()
				runs := filepath.Join(root, "runs")
				if resume {
					if err := worktree.EstablishRemoteBranch(ctx, opts); err != nil {
						t.Fatal(err)
					}
					jr, err := journal.Create(runs, journal.RunIdentity{RunID: id, Workflow: machine.Def.Name, WorkflowVersion: machine.Def.Version,
						WorkflowDigest: machine.Digest(), Gaggle: "web", Trigger: journal.Trigger{Kind: journal.TriggerSchedule}}, nil)
					if err != nil {
						t.Fatal(err)
					}
					for _, event := range []journal.Event{
						{Type: journal.EventStageFinished, Stage: "select", Status: "success", WorkspaceRevision: selection},
						{Type: journal.EventStageFinished, Stage: "establish", Status: "success", WorkspaceBranchBinding: binding,
							Outputs: map[string]any{WorkspaceBranchOutput: strings.TrimPrefix(binding.Ref, "refs/heads/")}},
					} {
						if err := jr.Append(event); err != nil {
							t.Fatal(err)
						}
					}
					jr.SetMachineState("author")
					if err := jr.Close(); err != nil {
						t.Fatal(err)
					}
				}
				manager, err := worktree.NewManager(filepath.Join(root, "workcopies"))
				if err != nil {
					t.Fatal(err)
				}
				var authored string
				r, err := New(Config{RunsDir: runs, ScratchDir: filepath.Join(root, "scratch"), Worktrees: manager, PinnedWorkspace: pinned,
					RepoCloneURL: func(apiv1.RepoRef) (string, error) { return repo, nil },
					NewDeterministic: func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
						return revisionExecutor{run: func(env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
							_, stage, _ := strings.Cut(env.TaskID, ":")
							switch stage {
							case "select":
								if resume {
									t.Fatal("resume reselected")
								}
								return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, WorkspaceRevision: selection}, nil
							case "establish":
								if err := worktree.EstablishRemoteBranch(ctx, opts); err != nil {
									return apiv1.ResultEnvelope{}, err
								}
								return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, WorkspaceBranchBinding: binding,
									Outputs: map[string]any{WorkspaceBranchOutput: strings.TrimPrefix(binding.Ref, "refs/heads/")}}, nil
							case "author":
								if env.WorkspaceBranchBinding == nil || *env.WorkspaceBranchBinding != *binding {
									t.Fatal("binding not transported")
								}
								reader, err := journal.OpenRead(filepath.Join(runs, id))
								if err != nil {
									t.Fatal(err)
								}
								events, err := reader.Events()
								if err != nil {
									t.Fatal(err)
								}
								restored, err := RestoredWorkspaceBranchBinding(events, machine, base, nil, "", id)
								if err != nil || restored == nil || *restored != *binding {
									t.Fatalf("workspace exposed before durable binding: %+v, %v", restored, err)
								}
								if head := strings.TrimSpace(gitOutput(t, env.Workspace, "rev-parse", "HEAD")); head != sha {
									t.Fatalf("author head = %s", head)
								}
								if err := os.WriteFile(filepath.Join(env.Workspace, "authored.txt"), []byte("persistent\n"), 0o600); err != nil {
									t.Fatal(err)
								}
								gitOutput(t, env.Workspace, "add", "authored.txt")
								gitOutput(t, env.Workspace, "commit", "-m", "author")
								authored = strings.TrimSpace(gitOutput(t, env.Workspace, "rev-parse", "HEAD"))
								env.WorkspaceBranchBinding.Ref = "refs/heads/attacker"
							case "publish":
								if env.WorkspaceBranchBinding.Ref != binding.Ref {
									t.Fatal("invocation mutation escaped")
								}
								if head := strings.TrimSpace(gitOutput(t, env.Workspace, "rev-parse", "HEAD")); head != authored {
									t.Fatal("authoring continuity lost")
								}
								tip, err := worktree.PublishRemoteBranchTip(ctx, opts, env.Workspace)
								return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, WorkspaceBranchTip: tip}, err
							}
							return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
						}}, nil
					},
				})
				if err != nil {
					t.Fatal(err)
				}
				var result Result
				if resume {
					result, err = r.Resume(ctx, ResumeInput{RunID: id, Machine: machine, RepoRef: base})
				} else {
					result, err = r.Start(ctx, StartInput{RunID: id, Machine: machine, RepoRef: base, Gaggle: "web", Trigger: journal.Trigger{Kind: journal.TriggerSchedule}})
				}
				if err != nil || result.Phase != journal.PhaseCompleted {
					t.Fatalf("result=%+v err=%v", result, err)
				}
				if tip := strings.TrimSpace(gitOutput(t, "", "--git-dir="+repo, "rev-parse", binding.Ref)); tip != authored {
					t.Fatal("remote publication missing")
				}
				reader, err := journal.OpenRead(filepath.Join(runs, id))
				if err != nil {
					t.Fatal(err)
				}
				events, err := reader.Events()
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, event := range events {
					found = found || event.Stage == "publish" && event.WorkspaceBranchTip == authored
				}
				if !found {
					t.Fatal("exact publication tip was not durably journaled")
				}
				if _, err := RestoredWorkspaceBranchBinding(events, machine, base, nil, "", id); err != nil {
					t.Fatalf("publication evidence did not restore: %v", err)
				}
				if source := strings.TrimSpace(gitOutput(t, "", "--git-dir="+repo, "rev-parse", rebindBranch)); source != sha {
					t.Fatal("source branch mutated")
				}
			})
		}
	}
}
