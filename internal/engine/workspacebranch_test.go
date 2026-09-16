package engine

import (
	"context"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	wf "github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/workspacebranch"
)

func TestOwnedBranchWorkflowDurabilityAndContinuity(t *testing.T) {
	spec := fixtureSpec("select", []apiv1.Task{
		{Name: "select", Type: apiv1.TaskDeterministic, Goal: "select", Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}, Next: "establish"},
		{Name: "establish", Type: apiv1.TaskDeterministic, Goal: "establish", Inputs: map[string]string{"kind": workspacebranch.KindEstablish}, Capabilities: []string{"repo:push"},
			Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}, Next: "author"},
		{Name: "author", Type: apiv1.TaskDeterministic, Goal: "author", Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceRepo}, Next: "publish"},
		{Name: "publish", Type: apiv1.TaskDeterministic, Goal: "publish", Inputs: map[string]string{"kind": workspacebranch.KindPublish}, Capabilities: []string{"repo:push"},
			Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceRepo}, Next: wf.TerminalComplete},
	}, nil)
	in := runInput("owned", spec)
	in.LiveJournal = true
	selected := selectedRevisionFixture()
	binding, err := workspacebranch.Expected(in.RepoRef, selected, in.BranchNamespace, in.WorkflowName, in.RunID)
	if err != nil {
		t.Fatal(err)
	}
	writer, runs := newLiveWriter(t)
	spaces := testWorkspaces(t)
	seen := false
	det := &fakeRunner{run: func(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
		switch {
		case strings.HasSuffix(env.TaskID, ":select"):
			return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, WorkspaceRevision: selected}, nil
		case strings.HasSuffix(env.TaskID, ":establish"):
			return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, WorkspaceBranchBinding: binding,
				Outputs: map[string]any{"workspaceBranch": strings.TrimPrefix(binding.Ref, "refs/heads/")}}, nil
		case strings.HasSuffix(env.TaskID, ":publish"):
			return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, WorkspaceBranchTip: strings.Repeat("b", 40)}, nil
		default:
			seen = true
			if env.WorkspaceBranchBinding == nil || *env.WorkspaceBranchBinding != *binding {
				t.Fatal("owned binding not transported")
			}
			events := liveEvents(t, runs, in.RunID)
			found := false
			for _, e := range events {
				found = found || e.WorkspaceBranchBinding != nil
			}
			if !found {
				t.Fatal("downstream ran before durable ownership")
			}
			env.WorkspaceBranchBinding.Ref = "refs/heads/attacker"
			return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
		}
	}}
	projection := executeLive(t, in, &Activities{Det: det, Workspaces: spaces, Journal: writer}, false)
	if !seen {
		t.Fatal("author did not run")
	}
	for _, req := range spaces.requests {
		if req.Stage == "author" && (req.WorkspaceBranch != strings.TrimPrefix(binding.Ref, "refs/heads/") || req.WorkspaceBranchBinding == nil) {
			t.Fatalf("workspace ownership transport = %+v", req)
		}
	}
	events := liveEvents(t, runs, in.RunID)
	machine, err := wf.Compile(wf.Definition{Name: in.WorkflowName, Version: in.Version, Spec: spec})
	if err != nil {
		t.Fatal(err)
	}
	restored, err := runner.RestoredWorkspaceBranchBinding(events, machine, in.RepoRef, in.AdditionalRepos, in.BranchNamespace, in.RunID)
	if err != nil || restored == nil || *restored != *binding {
		t.Fatalf("restore = %+v, %v", restored, err)
	}
	if divergence, err := DiffLiveJournal(events, projection); err != nil || len(divergence) != 0 {
		t.Fatalf("ownership projection divergence = %v, %v", divergence, err)
	}
	published := false
	for _, event := range events {
		if event.WorkspaceBranchBinding != nil && event.Type != journal.EventStageFinished {
			t.Fatal("non-normative ownership")
		}
		published = published || event.Stage == "publish" && event.WorkspaceBranchTip == strings.Repeat("b", 40)
	}
	if !published {
		t.Fatal("exact publication tip was not durably journaled")
	}
}
