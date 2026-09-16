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
	"github.com/goobers/goobers/internal/worktree"
)

func TestLegacyPinnedReadonlyPreservesOnlyWritableCommits(t *testing.T) {
	repo := newFixtureRepo(t)
	manager, err := worktree.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tasks := []apiv1.Task{}
	for i, name := range []string{"author", "inspect", "inspect-again", "verify"} {
		mode, next := apiv1.WorkspaceRepo, workflow.TerminalComplete
		if i == 1 || i == 2 {
			mode = apiv1.WorkspaceRepoReadOnly
		}
		if i < 3 {
			next = []string{"inspect", "inspect-again", "verify"}[i]
		}
		tasks = append(tasks, apiv1.Task{Name: name, Type: apiv1.TaskDeterministic, Goal: name,
			Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: mode}, Next: next})
	}
	machine, err := workflow.Compile(workflow.Definition{Name: "pinned-inspection", Version: 1, Spec: apiv1.WorkflowSpec{
		Gaggle: "web", Start: "author", Tasks: tasks, Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var authored string
	r, err := New(Config{RunsDir: t.TempDir(), Worktrees: manager, PinnedWorkspace: true,
		RepoCloneURL: func(apiv1.RepoRef) (string, error) { return repo, nil },
		NewDeterministic: func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
			return revisionExecutor{run: func(env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
				_, stage, _ := strings.Cut(env.TaskID, ":")
				if stage == "author" {
					gitOutput(t, env.Workspace, "commit", "--allow-empty", "-m", "preserved writable state")
					authored = strings.TrimSpace(gitOutput(t, env.Workspace, "rev-parse", "HEAD"))
				} else {
					if actual := strings.TrimSpace(gitOutput(t, env.Workspace, "rev-parse", "HEAD")); actual != authored {
						t.Fatalf("%s inherited inspection commit or lost writable commit", stage)
					}
					for _, name := range []string{"untracked", "build.ignored", ".gitignore"} {
						if _, err := os.Stat(filepath.Join(env.Workspace, name)); !os.IsNotExist(err) {
							t.Fatalf("%s retained %s: %v", stage, name, err)
						}
					}
				}
				if strings.HasPrefix(stage, "inspect") {
					if branch := strings.TrimSpace(gitOutput(t, env.Workspace, "rev-parse", "--abbrev-ref", "HEAD")); branch != "HEAD" {
						t.Fatal("inspection did not detach")
					}
					gitOutput(t, env.Workspace, "commit", "--allow-empty", "-m", "discard inspection state")
					for name, content := range map[string]string{"untracked": "discard", ".gitignore": "*.ignored\n", "build.ignored": "discard"} {
						if err := os.WriteFile(filepath.Join(env.Workspace, name), []byte(content), 0o600); err != nil {
							t.Fatal(err)
						}
					}
				}
				return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
			}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := r.Start(context.Background(), StartInput{RunID: "pinned-readonly", Machine: machine, Gaggle: "web",
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	})
	if err != nil || result.Phase != journal.PhaseCompleted {
		t.Fatalf("pinned inspection result = %+v, %v", result, err)
	}
}
