package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/testgit"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

type revisionExecutor struct {
	run func(apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error)
}

func (e revisionExecutor) Run(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	return e.run(env)
}

func revisionMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	m, err := workflow.Compile(workflow.Definition{Name: "revision-test", Version: 1, Spec: apiv1.WorkflowSpec{
		Gaggle: "acme-web", Start: "select",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerSchedule, Schedule: "0 * * * *"}},
		Tasks: []apiv1.Task{
			{Name: "select", Type: apiv1.TaskDeterministic, Goal: "select immutable revision", Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}, Next: "inspect"},
			{Name: "inspect", Type: apiv1.TaskDeterministic, Goal: "inspect selected revision", Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceRepoReadOnly}, Next: "verify"},
			{Name: "verify", Type: apiv1.TaskDeterministic, Goal: "verify stateless inspection", Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceRepoReadOnly}, Next: workflow.TerminalComplete},
		},
	}}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func revisionFixture(sha string) *apiv1.WorkspaceRevision {
	return &apiv1.WorkspaceRevision{
		Repository: apiv1.RepositoryIdentity{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"},
		CommitSHA:  sha, SourceRef: rebindBranch,
	}
}

func TestSelectedRevisionLocalRunAndResume(t *testing.T) {
	for _, resume := range []bool{false, true} {
		for _, pinned := range []bool{false, true} {
			name := "disposable"
			if pinned {
				name = "pinned"
			}
			if resume {
				name += "-resume"
			}
			t.Run(name, func(t *testing.T) {
				root := t.TempDir()
				repo := newRebindFixtureRepo(t)
				sha := strings.TrimSpace(gitOutput(t, "", "--git-dir="+repo, "rev-parse", rebindBranch))
				revision := revisionFixture(sha)
				machine := revisionMachine(t)
				manager, err := worktree.NewManager(filepath.Join(root, "workcopies"))
				if err != nil {
					t.Fatal(err)
				}
				runs := filepath.Join(root, "runs")
				observed := 0
				r, err := New(Config{
					Worktrees: manager, RunsDir: runs, ScratchDir: filepath.Join(root, "scratch"),
					PinnedWorkspace: pinned,
					RepoCloneURL:    func(apiv1.RepoRef) (string, error) { return repo, nil },
					NewDeterministic: func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
						return revisionExecutor{run: func(env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
							_, stage, _ := strings.Cut(env.TaskID, ":")
							if stage == "select" {
								if resume {
									t.Fatal("resume repolled selector")
								}
								return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, WorkspaceRevision: revision}, nil
							}
							observed++
							if env.WorkspaceRevision == nil || env.WorkspaceRevision.CommitSHA != sha {
								t.Fatalf("invocation lost selected authority: %+v", env.WorkspaceRevision)
							}
							env.WorkspaceRevision.CommitSHA = strings.Repeat("f", 40)
							if actual := strings.TrimSpace(gitOutput(t, env.Workspace, "rev-parse", "HEAD")); actual != sha {
								t.Fatalf("HEAD = %s, want %s", actual, sha)
							}
							if branch := strings.TrimSpace(gitOutput(t, env.Workspace, "rev-parse", "--abbrev-ref", "HEAD")); branch != "HEAD" {
								t.Fatalf("not detached: %s", branch)
							}
							if _, err := os.Stat(filepath.Join(env.Workspace, "leftover.txt")); !os.IsNotExist(err) {
								t.Fatalf("stage retained untracked state: %v", err)
							}
							if _, err := os.Stat(filepath.Join(env.Workspace, rebindMarkerFile)); err != nil {
								t.Fatalf("selected source content missing: %v", err)
							}
							if err := os.WriteFile(filepath.Join(env.Workspace, "leftover.txt"), []byte("discard me"), 0o600); err != nil {
								t.Fatal(err)
							}
							return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
						}}, nil
					},
				})
				if err != nil {
					t.Fatal(err)
				}
				ref := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"}
				const id = "revision-run"
				var result Result
				if resume {
					jr, createErr := journal.Create(runs, journal.RunIdentity{
						RunID: id, Workflow: machine.Def.Name, WorkflowVersion: machine.Def.Version,
						WorkflowDigest: machine.Digest(), Gaggle: "acme-web",
						Trigger: journal.Trigger{Kind: journal.TriggerSchedule},
					}, nil)
					if createErr != nil {
						t.Fatal(createErr)
					}
					jr.SetMachineState("select")
					if err := jr.Append(journal.Event{Type: journal.EventStageStarted, Stage: "select", Attempt: 1}); err != nil {
						t.Fatal(err)
					}
					if err := jr.Append(journal.Event{Type: journal.EventStageFinished, Stage: "select", Attempt: 1, Status: "success", WorkspaceRevision: revision}); err != nil {
						t.Fatal(err)
					}
					if err := jr.Close(); err != nil {
						t.Fatal(err)
					}
					result, err = r.Resume(context.Background(), ResumeInput{RunID: id, Machine: machine, RepoRef: ref})
				} else {
					result, err = r.Start(context.Background(), StartInput{
						RunID: id, Machine: machine, RepoRef: ref, Gaggle: "acme-web",
						Trigger: journal.Trigger{Kind: journal.TriggerSchedule},
					})
				}
				if err != nil || result.Phase != journal.PhaseCompleted || observed != 2 {
					t.Fatalf("result=%+v err=%v inspections=%d", result, err, observed)
				}
				rd, err := journal.OpenRead(filepath.Join(runs, id))
				if err != nil {
					t.Fatal(err)
				}
				events, err := rd.Events()
				if err != nil {
					t.Fatal(err)
				}
				restored, err := r.restoreWorkspaceRevision(events, StartInput{Machine: machine, RepoRef: ref})
				if err != nil || !reflect.DeepEqual(restored, revision) {
					t.Fatalf("restored=%+v err=%v", restored, err)
				}
			})
		}
	}
}

func TestSelectedRevisionReplayFailsClosed(t *testing.T) {
	machine := revisionMachine(t)
	ref := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"}
	selected := revisionFixture(strings.Repeat("a", 40))
	other := revisionFixture(strings.Repeat("b", 40))
	r := &Runner{}
	first := journal.Event{Type: journal.EventStageFinished, Stage: "select", Status: "success", WorkspaceRevision: selected}
	for _, tc := range []struct {
		name      string
		events    []journal.Event
		wantError bool
	}{
		{"legacy", []journal.Event{{Type: journal.EventStageFinished, Stage: "select", Status: "success"}}, false},
		{"identical", []journal.Event{first, first}, false},
		{"conflict", []journal.Event{first, {Type: journal.EventStageFinished, Stage: "verify", Status: "success", WorkspaceRevision: other}}, true},
		{"unknown producer", []journal.Event{{Type: journal.EventStageFinished, Stage: "untrusted", Status: "success", WorkspaceRevision: selected}}, true},
		{"wrong event", []journal.Event{{Type: journal.EventRunnerAnnotation, Stage: "select", WorkspaceRevision: selected}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := r.restoreWorkspaceRevision(tc.events, StartInput{Machine: machine, RepoRef: ref})
			if (err != nil) != tc.wantError {
				t.Fatalf("error=%v wantError=%v", err, tc.wantError)
			}
		})
	}
}

func TestSelectedRevisionRejectsBranchAndSyncBase(t *testing.T) {
	r := &Runner{}
	in := StartInput{workspaceRevision: revisionFixture(strings.Repeat("a", 40))}
	for _, tc := range []struct {
		branch string
		sync   bool
	}{{branch: "goobers/test/run"}, {sync: true}} {
		if _, err := r.createRevisionWorkspace(context.Background(), in, "inspect", tc.sync, tc.branch); err == nil {
			t.Fatal("accepted writable control on selected read-only workspace")
		}
	}
}

func TestSelectedRevisionProducerAuthority(t *testing.T) {
	ref := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"}
	selected := revisionFixture(strings.Repeat("a", 40))
	r := &Runner{}
	for _, tc := range []struct {
		name          string
		kind          apiv1.TaskType
		status        apiv1.ResultStatus
		revision      *apiv1.WorkspaceRevision
		wantError     bool
		wantSelection bool
	}{
		{"deterministic", apiv1.TaskDeterministic, apiv1.ResultSuccess, selected, false, true},
		{"agentic", apiv1.TaskAgentic, apiv1.ResultSuccess, selected, true, false},
		{"failure", apiv1.TaskDeterministic, apiv1.ResultFailure, selected, false, false},
		{"legacy", apiv1.TaskAgentic, apiv1.ResultSuccess, nil, false, false},
		{"malformed", apiv1.TaskDeterministic, apiv1.ResultSuccess, revisionFixture("main"), true, false},
		{"unauthorized", apiv1.TaskDeterministic, apiv1.ResultSuccess, &apiv1.WorkspaceRevision{
			Repository: apiv1.RepositoryIdentity{Provider: apiv1.ProviderGitHub, Owner: "other", Name: "web"},
			CommitSHA:  strings.Repeat("a", 40),
		}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := r.acceptWorkspaceRevision(StartInput{RepoRef: ref}, apiv1.Task{Type: tc.kind},
				apiv1.ResultEnvelope{Status: tc.status, WorkspaceRevision: tc.revision})
			if (err != nil) != tc.wantError || (got != nil) != tc.wantSelection {
				t.Fatalf("selected=%+v err=%v", got, err)
			}
		})
	}
}

func TestSelectedRevisionParallelBranchesReceiveIndependentTrees(t *testing.T) {
	def := parallelRunnerMachine(t, 3, apiv1.WorkspaceRepoReadOnly).Def
	def.Spec.Start = "select"
	for i := range def.Spec.Tasks {
		if strings.HasPrefix(def.Spec.Tasks[i].Name, "lens-") {
			def.Spec.Tasks[i].Run.Workspace = apiv1.WorkspaceRepoReadOnly
		}
	}
	def.Spec.Tasks = append(def.Spec.Tasks, apiv1.Task{
		Name: "select", Type: apiv1.TaskDeterministic, Goal: "select exact revision",
		Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}, Next: "fan",
	})
	machine, err := workflow.Compile(def, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}
	repo := newRebindFixtureRepo(t)
	sha := strings.TrimSpace(gitOutput(t, "", "--git-dir="+repo, "rev-parse", rebindBranch))
	root := t.TempDir()
	manager, err := worktree.NewManager(filepath.Join(root, "workcopies"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	arrived := make(chan string, 3)
	release := make(chan struct{})
	paths := make(chan []string, 1)
	go func() {
		var seen []string
		for len(seen) != 3 {
			select {
			case path := <-arrived:
				seen = append(seen, path)
			case <-ctx.Done():
				paths <- seen
				close(release)
				return
			}
		}
		paths <- seen
		close(release)
	}()
	r, err := New(Config{
		Worktrees: manager, RunsDir: filepath.Join(root, "runs"), ScratchDir: filepath.Join(root, "scratch"),
		RepoCloneURL: func(apiv1.RepoRef) (string, error) { return repo, nil },
		NewDeterministic: func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
			return revisionExecutor{run: func(env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
				_, stage, _ := strings.Cut(env.TaskID, ":")
				if stage == "select" {
					return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, WorkspaceRevision: revisionFixture(sha)}, nil
				}
				if strings.HasPrefix(stage, "lens-") {
					cmd := testgit.Command("rev-parse", "HEAD")
					cmd.Dir = env.Workspace
					head, err := cmd.CombinedOutput()
					if err != nil {
						return apiv1.ResultEnvelope{}, fmt.Errorf("read parallel HEAD: %w", err)
					}
					if strings.TrimSpace(string(head)) != sha {
						return apiv1.ResultEnvelope{}, fmt.Errorf("parallel HEAD=%s, expected %s", head, sha)
					}
					arrived <- env.Workspace
					select {
					case <-release:
					case <-ctx.Done():
						return apiv1.ResultEnvelope{}, ctx.Err()
					}
				}
				return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Outputs: map[string]any{"inspected": true}}, nil
			}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := r.Start(ctx, StartInput{
		RunID: "parallel-revision", Machine: machine, Gaggle: "demo",
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	})
	cancel()
	seen := <-paths
	if err != nil || result.Phase != journal.PhaseCompleted || len(seen) != 3 {
		t.Fatalf("result=%+v err=%v concurrent workspaces=%v", result, err, seen)
	}
	if seen[0] == seen[1] || seen[0] == seen[2] || seen[1] == seen[2] {
		t.Fatalf("parallel branches shared a tree: %v", seen)
	}
}
func TestSelectedRevisionForkWorkspacesCoexist(t *testing.T) {
	base := newFixtureRepo(t)
	fork := newRebindFixtureRepo(t)
	sha := strings.TrimSpace(gitOutput(t, "", "--git-dir="+fork, "rev-parse", rebindBranch))
	selected := revisionFixture(sha)
	selected.Repository.Owner = "fork-owner"
	root := t.TempDir()
	manager, err := worktree.NewManager(filepath.Join(root, "workcopies"))
	if err != nil {
		t.Fatal(err)
	}
	source := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "fork-owner", Name: "web", Branch: "main"}
	r := &Runner{cfg: Config{
		Worktrees: manager, AdditionalRepos: []apiv1.RepoRef{source},
		RepoCloneURL: func(ref apiv1.RepoRef) (string, error) {
			if ref.Owner == source.Owner {
				return fork, nil
			}
			return base, nil
		},
	}}
	in := StartInput{
		RunID: "fork-readonly", Machine: revisionMachine(t),
		RepoRef:           apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
		workspaceRevision: selected,
	}
	ctx := context.Background()
	first, err := r.createRevisionWorkspace(ctx, in, "first", false, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := first.Remove(ctx); err != nil {
			t.Error(err)
		}
	}()
	second, err := r.createRevisionWorkspace(ctx, in, "second", false, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := second.Remove(ctx); err != nil {
			t.Error(err)
		}
	}()
	if first.path == second.path {
		t.Fatal("read-only branches share a mutable working directory")
	}
	for _, workspace := range []*stageWorkspace{first, second} {
		got, err := workspace.worktree.HeadSHA(ctx)
		if err != nil || got != sha {
			t.Fatalf("HEAD=%s err=%v expected=%s", got, err, sha)
		}
	}
}
