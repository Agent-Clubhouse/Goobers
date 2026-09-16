//go:build integration

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/journalclient"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/workspacebranch"
	"github.com/goobers/goobers/internal/worktree"
)

type workspaceCleanupAudit struct {
	events []journal.Event
	err    error
}

func (a *workspaceCleanupAudit) Append(event journal.Event) error {
	a.events = append(a.events, event)
	return a.err
}

func TestIntegrationOwnedWorkspaceTerminalCleanup(t *testing.T) {
	for _, scenario := range []string{"published", "old-publication", "lost-publication-ack", "missing-ownership", "altered-ownership", "invalid-producer", "audit-failure"} {
		t.Run(scenario, func(t *testing.T) {
			input, env, target, source := ownedBackendFixture(t)
			env.BranchNamespace = ""
			spec := apiv1.WorkflowSpec{Gaggle: "web", Start: "select", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
				Tasks: []apiv1.Task{
					{Name: "select", Type: apiv1.TaskDeterministic, Goal: "select", Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}, Next: "establish"},
					{Name: "establish", Type: apiv1.TaskDeterministic, Goal: "establish", Inputs: map[string]string{"kind": workspacebranch.KindEstablish},
						Capabilities: []string{"repo:push"}, Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}, Next: "publish"},
					{Name: "publish", Type: apiv1.TaskDeterministic, Goal: "publish", Inputs: map[string]string{"kind": workspacebranch.KindPublish},
						Capabilities: []string{"repo:push"}, Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceRepo}, Next: workflow.TerminalComplete},
				}}
			machine := compileCredentialPlaneMachine(t, spec)
			env.WorkflowID = machine.Def.Name
			layout := instance.NewLayout(t.TempDir()).ForGaggle("web")
			gaggle := map[string]any{
				"apiVersion": "goobers.dev/v1alpha1", "kind": "Gaggle", "metadata": map[string]string{"name": "web"},
				"spec": apiv1.GaggleSpec{Project: input.GaggleProject, AdditionalRepos: input.AdditionalRepos,
					Backlog:   apiv1.BacklogRef{Provider: apiv1.ProviderGitHub, Project: "owner/base"},
					Isolation: apiv1.GaggleIsolation{Namespace: "web"}},
			}
			dir := filepath.Join(layout.ConfigDir(), "gaggles", "web")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(gaggle)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "gaggle.yaml"), data, 0o600); err != nil {
				t.Fatal(err)
			}
			manifest := "apiVersion: goobers.dev/v1alpha1\nkind: Manifest\nmetadata:\n  name: fixture\nspec:\n  instance:\n    name: fixture\n    environment: dev\n  gaggles: [web]\n"
			if err := os.WriteFile(filepath.Join(layout.ConfigDir(), "manifest.yaml"), []byte(manifest), 0o600); err != nil {
				t.Fatal(err)
			}
			writePinnedRun(t, layout, "web", env.RunID, machine, nil)
			writer, _, err := journal.Recover(filepath.Join(layout.RunsDir(), env.RunID))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = writer.Close() })
			appendEvent := func(event journal.Event) {
				t.Helper()
				if err := writer.Append(event); err != nil {
					t.Fatal(err)
				}
			}
			if scenario != "missing-ownership" {
				appendEvent(journal.Event{Type: journal.EventStageFinished, Stage: "select", Status: "success", WorkspaceRevision: env.WorkspaceRevision})
			}
			backend, err := buildDeterministicExecutor(input)
			if err != nil {
				t.Fatal(err)
			}
			established, err := backend.Run(context.Background(), env, *spec.Tasks[1].Run)
			if err != nil {
				t.Fatal(err)
			}
			if scenario != "missing-ownership" {
				evidence := established.WorkspaceBranchBinding.DeepCopy()
				if scenario == "altered-ownership" {
					evidence.Repository.Name = "unowned"
				}
				appendEvent(journal.Event{Type: journal.EventStageFinished, Stage: "establish", Status: "success",
					WorkspaceBranchBinding: evidence, Outputs: established.Outputs})
			}
			branch := strings.TrimPrefix(established.WorkspaceBranchBinding.Ref, "refs/heads/")
			workspace := filepath.Join(t.TempDir(), "author")
			podRevisionGit(t, "", "clone", "--branch", branch, target, workspace)
			podRevisionGit(t, workspace, "commit", "--allow-empty", "-m", "published experiment")
			env.Workspace, env.WorkspaceBranchBinding = workspace, established.WorkspaceBranchBinding
			env.Inputs["kind"] = workspacebranch.KindPublish
			published, err := backend.Run(context.Background(), env, *spec.Tasks[2].Run)
			if err != nil || published.WorkspaceBranchTip == "" {
				t.Fatalf("publication has no typed acknowledgment: %+v %v", published, err)
			}
			if scenario != "lost-publication-ack" && scenario != "missing-ownership" {
				tip := published.WorkspaceBranchTip
				stage := "publish"
				if scenario == "old-publication" {
					tip = ""
				}
				if scenario == "invalid-producer" {
					stage = "select"
				}
				appendEvent(journal.Event{Type: journal.EventStageFinished, Stage: stage, Status: "success", WorkspaceBranchTip: tip})
			}
			ownership, err := journalclient.NewFileCrossRun(layout).BranchOwnership(context.Background(), journalclient.BranchOwnershipRequest{
				Gaggle: "web", TargetRunID: env.RunID, Workflow: machine.Def.Name, Branch: branch,
			})
			if err != nil || ownership.Owner != nil || ownership.Reason != "owned-workspace-cleanup-required" {
				t.Fatalf("legacy sweep accepted sandbox: %+v, %v", ownership, err)
			}
			sourceTip := podRevisionGit(t, source, "-c", "safe.bareRepository=all", "rev-parse", "HEAD")
			prepare, err := buildTerminalBranchPreparer(layout, input.Config, input.GaggleProject, input.SharedRegistry, nil)
			if err != nil {
				t.Fatal(err)
			}
			audit := &workspaceCleanupAudit{}
			if scenario == "audit-failure" {
				audit.err = errors.New("audit unavailable")
			}
			err = prepare(env.RunID, journal.PhaseAborted, audit)
			wantErr := scenario == "altered-ownership" || scenario == "invalid-producer" || scenario == "audit-failure"
			if (err != nil) != wantErr || audit.err != nil && !errors.Is(err, audit.err) {
				t.Fatalf("cleanup error = %v; want error %v", err, wantErr)
			}
			want := worktree.CleanupDeleted
			if scenario == "old-publication" || scenario == "missing-ownership" {
				want = worktree.CleanupOwnershipMissing
			}
			if scenario == "altered-ownership" || scenario == "invalid-producer" {
				want = worktree.CleanupOwnershipInvalid
			}
			if scenario == "lost-publication-ack" {
				want = worktree.CleanupTipChanged
			}
			if len(audit.events) == 0 || audit.events[0].Runner["outcome"] != string(want) {
				t.Fatalf("cleanup audit = %+v; want %s", audit.events, want)
			}
			if want != worktree.CleanupDeleted {
				observed := podRevisionGit(t, target, "-c", "safe.bareRepository=all", "rev-parse", established.WorkspaceBranchBinding.Ref)
				if observed != published.WorkspaceBranchTip {
					t.Fatal("ambiguous branch was changed")
				}
			}
			if scenario == "published" {
				audit.events = nil
				if err := prepare(env.RunID, journal.PhaseCompleted, audit); err != nil {
					t.Fatal(err)
				}
				if audit.events[0].Runner["outcome"] != string(worktree.CleanupAlreadyAbsent) {
					t.Fatalf("repeat cleanup = %+v", audit.events)
				}
			}
			if after := podRevisionGit(t, source, "-c", "safe.bareRepository=all", "rev-parse", "HEAD"); after != sourceTip {
				t.Fatal("terminal cleanup mutated source")
			}
		})
	}
}
