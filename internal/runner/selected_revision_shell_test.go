package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

// Re-exec the test binary so the result-file contract crosses a real process
// boundary on Windows and Unix without relying on shell-specific quoting.
func TestSelectedRevisionShellHelper(t *testing.T) {
	args := os.Args
	for len(args) > 0 && args[0] != "--selected-revision-helper" {
		args = args[1:]
	}
	if len(args) == 0 {
		return
	}
	args = args[1:]
	switch args[0] {
	case "select":
		if err := os.WriteFile("selection.json", []byte(args[1]), 0o600); err != nil {
			t.Fatal(err)
		}
	case "inspect":
		for _, check := range []struct {
			args []string
			want string
		}{
			{[]string{"rev-parse", "HEAD"}, args[1]},
			{[]string{"rev-parse", "--abbrev-ref", "HEAD"}, "HEAD"},
			{[]string{"status", "--porcelain", "--ignored"}, ""},
		} {
			out, err := exec.Command("git", check.args...).CombinedOutput()
			if err != nil || strings.TrimSpace(string(out)) != check.want {
				t.Fatalf("git %v: %s, %v; want %q", check.args, out, err, check.want)
			}
		}
		if _, err := os.Stat(rebindMarkerFile); err != nil {
			t.Fatal(err)
		}
		for name, content := range map[string]string{
			"README.md": "dirty tracked content\n", "untracked.txt": "discard\n",
			".gitignore": "*.ignored\n", "build.ignored": "discard\n",
		} {
			if err := os.WriteFile(name, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	default:
		t.Fatalf("unknown helper operation %q", args[0])
	}
}

func TestSelectedRevisionShellResultFileFunctional(t *testing.T) {
	for _, pinned := range []bool{false, true} {
		for _, scenario := range []string{"exact-after-source-moves", "unauthorized", "malformed", "conflicting", "no-work"} {
			t.Run(fmt.Sprintf("%s/pinned=%t", scenario, pinned), func(t *testing.T) {
				repo := newRebindFixtureRepo(t)
				sha := strings.TrimSpace(gitOutput(t, "", "--git-dir="+repo, "rev-parse", rebindBranch))
				mainSHA := strings.TrimSpace(gitOutput(t, "", "--git-dir="+repo, "rev-parse", "main"))
				revision := revisionFixture(sha)
				if scenario == "unauthorized" {
					revision.Repository.Owner = "unconfigured-fork"
				}
				if scenario == "malformed" {
					revision.CommitSHA = "main"
				}
				payload := map[string]any{"workspaceRevision": revision, "number": "42"}
				if scenario == "no-work" {
					payload["noWork"] = true
				}
				data, err := json.Marshal(payload)
				if err != nil {
					t.Fatal(err)
				}
				binary, err := os.Executable()
				if err != nil {
					t.Fatal(err)
				}
				command := func(args ...string) []string {
					return append([]string{binary, "-test.run=^TestSelectedRevisionShellHelper$", "--", "--selected-revision-helper"}, args...)
				}
				machine := revisionMachine(t)
				def := machine.Def
				def.Spec.Tasks[0].Run.Command = command("select", string(data))
				def.Spec.Tasks[0].Inputs = map[string]string{"resultFile": "selection.json", "timeout": "30s"}
				def.Spec.Tasks[1].Run.Command = command("inspect", sha)
				def.Spec.Tasks[2].Run.Command = command("inspect", sha)
				if scenario == "conflicting" {
					other := revisionFixture(mainSHA)
					second, err := json.Marshal(map[string]any{"workspaceRevision": other})
					if err != nil {
						t.Fatal(err)
					}
					def.Spec.Tasks[1].Run.Workspace = apiv1.WorkspaceScratch
					def.Spec.Tasks[1].Run.Command = command("select", string(second))
					def.Spec.Tasks[1].Inputs = map[string]string{"resultFile": "selection.json", "timeout": "30s"}
				}
				machine, err = workflow.Compile(def, workflow.WithPreviewFeatures(true))
				if err != nil {
					t.Fatal(err)
				}
				// The recorded source branch no longer names the selected commit.
				gitOutput(t, "", "--git-dir="+repo, "update-ref", "refs/heads/"+rebindBranch, mainSHA)
				root := t.TempDir()
				manager, err := worktree.NewManager(filepath.Join(root, "workcopies"))
				if err != nil {
					t.Fatal(err)
				}
				resolver, err := credentials.NewResolver(nil)
				if err != nil {
					t.Fatal(err)
				}
				r, err := New(Config{
					RunsDir: filepath.Join(root, "runs"), ScratchDir: filepath.Join(root, "scratch"),
					Worktrees: manager, PinnedWorkspace: pinned,
					RepoCloneURL: func(apiv1.RepoRef) (string, error) { return repo, nil },
					NewDeterministic: func(rec ArtifactRecorder, reg SecretRegistrar) (invoke.Deterministic, error) {
						injector, err := credentials.NewInjector(resolver, nil, reg)
						if err != nil {
							return nil, err
						}
						return executor.NewShellExecutor(injector, rec)
					},
				})
				if err != nil {
					t.Fatal(err)
				}
				result, err := r.Start(context.Background(), StartInput{
					RunID: "shell-revision", Machine: machine, Gaggle: "acme-web",
					RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
					Trigger: journal.Trigger{Kind: journal.TriggerManual},
				})
				wantCode := map[string]string{"unauthorized": "workspace_revision_unauthorized",
					"malformed": "workspace_revision_invalid", "conflicting": "workspace_revision_conflict"}[scenario]
				if wantCode == "" && err != nil {
					t.Fatal(err)
				}
				if wantCode != "" {
					var coded stageCodedError
					if !errors.As(err, &coded) || coded.StageErrorCode() != wantCode {
						t.Fatalf("returned error = %v, want %s", err, wantCode)
					}
				}
				wantPhase := journal.PhaseFailed
				if scenario == "exact-after-source-moves" || scenario == "no-work" {
					wantPhase = journal.PhaseCompleted
				}
				if result.Phase != wantPhase {
					t.Fatalf("phase = %s, want %s; result=%+v", result.Phase, wantPhase, result)
				}
				reader, err := journal.OpenRead(filepath.Join(root, "runs", "shell-revision"))
				if err != nil {
					t.Fatal(err)
				}
				events, err := reader.Events()
				if err != nil {
					t.Fatal(err)
				}
				accepted, inspected := 0, 0
				for _, event := range events {
					if event.Type == journal.EventStageFinished {
						if event.WorkspaceRevision != nil {
							accepted++
							if event.WorkspaceRevision.CommitSHA != sha {
								t.Fatalf("accepted substituted revision: %+v", event)
							}
						}
						if event.Stage == "verify" && event.Status == "success" {
							inspected++
						}
					}
				}
				wantAccepted := 0
				if scenario == "exact-after-source-moves" || scenario == "conflicting" {
					wantAccepted = 1
				}
				if accepted != wantAccepted {
					t.Fatalf("accepted controls = %d, want %d", accepted, wantAccepted)
				}
				if scenario == "exact-after-source-moves" && inspected != 1 {
					t.Fatal("second stateless inspection did not complete")
				}
				if scenario != "exact-after-source-moves" && inspected != 0 {
					t.Fatal("refused or no-work selection reached downstream inspection")
				}
				if wantCode != "" && result.FailureCode != wantCode {
					t.Fatalf("failure code = %q, want %q", result.FailureCode, wantCode)
				}
				if actual := strings.TrimSpace(gitOutput(t, "", "--git-dir="+repo, "rev-parse", rebindBranch)); actual != mainSHA {
					t.Fatal("inspection changed the source branch")
				}
			})
		}
	}
}
