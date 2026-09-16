package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/workspacebranch"
)

func ownedCredentialFixture(t *testing.T) (*daemonCredentialService, httpapi.CredentialResolveRequest) {
	t.Helper()
	spec := credentialPlaneSpec()
	spec.Start, spec.Gates = "select", nil
	spec.Tasks = []apiv1.Task{
		{Name: "select", Type: apiv1.TaskDeterministic, Goal: "select", Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}, Next: "establish"},
		{Name: "establish", Type: apiv1.TaskDeterministic, Goal: "establish", Inputs: map[string]string{"kind": workspacebranch.KindEstablish},
			Capabilities: []string{"repo:push"}, Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch}, Next: "author"},
		{Name: "author", Type: apiv1.TaskDeterministic, Goal: "author", Capabilities: []string{"repo:push"},
			Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceRepo}, Next: workflow.TerminalComplete},
	}
	machine := compileCredentialPlaneMachine(t, spec)
	service, _, runID := newCredentialPlaneFixture(t, machine)
	base := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"}
	selection := &apiv1.WorkspaceRevision{Repository: apiv1.RepositoryIdentity{Provider: base.Provider, Owner: base.Owner, Name: base.Name}, CommitSHA: strings.Repeat("a", 40)}
	binding, err := workspacebranch.Expected(base, selection, "", machine.Def.Name, runID)
	if err != nil {
		t.Fatal(err)
	}
	service.Replace(credentialPlaneDefinitions{Scopes: map[string]credentialGaggleScope{"web": {Project: base}}})
	t.Setenv("OWNED_CHECKOUT_TOKEN", "owned-checkout-only-fixture")
	service.config = &instance.Config{Repos: []instance.RepoRef{{Provider: "github", Owner: base.Owner, Name: base.Name, Token: instance.TokenRef{Env: "OWNED_CHECKOUT_TOKEN"}}}}
	service.buildSources = nil
	run, _, err := journal.Recover(filepath.Join(service.layout.ForGaggle("web").RunsDir(), runID))
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []journal.Event{
		{Type: journal.EventStageFinished, Stage: "select", Status: "success", WorkspaceRevision: selection},
		{Type: journal.EventStageFinished, Stage: "establish", Status: "success", WorkspaceBranchBinding: binding,
			Outputs: map[string]any{"workspaceBranch": strings.TrimPrefix(binding.Ref, "refs/heads/")}},
	} {
		if err := run.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	return service, httpapi.CredentialResolveRequest{RunID: runID, Stage: "author", WorkspaceBranchBinding: binding}
}

func TestOwnedBranchCredentialPlaneBoundary(t *testing.T) {
	ctx := context.Background()
	service, request := ownedCredentialFixture(t)
	got, err := service.Resolve(ctx, request)
	if err != nil || len(got.Credentials) != 1 || got.Credentials[0].Capability != httpapi.WorkspaceBranchCheckoutCapability {
		t.Fatalf("owned checkout credentials = %+v, %v", got, err)
	}
	for _, stage := range []string{"author", "establish"} {
		plain := httpapi.CredentialResolveRequest{RunID: request.RunID, Stage: stage, Capabilities: []string{"repo:push"}}
		got, err := service.Resolve(ctx, plain)
		if err != nil || len(got.Credentials) != 0 {
			t.Fatalf("%s obtained raw publication credentials: %+v, %v", stage, got, err)
		}
	}
	if _, err := service.Resolve(ctx, httpapi.CredentialResolveRequest{
		RunID: request.RunID, Stage: "author", Capabilities: []string{"agent:model"},
	}); err == nil {
		t.Fatal("owned restriction authorized an undeclared capability")
	}
	request.WorkspaceBranchBinding = request.WorkspaceBranchBinding.DeepCopy()
	request.WorkspaceBranchBinding.Ref = "refs/heads/goobers/other/run"
	if _, err := service.Resolve(ctx, request); err == nil {
		t.Fatal("forged branch authorized")
	}
}
