package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

func revisionCredentialFixture(t *testing.T, fork, accepted bool) (*daemonCredentialService, *journal.RegistryScrubber, httpapi.CredentialResolveRequest) {
	t.Helper()
	spec := credentialPlaneSpec()
	spec.Start = "select"
	spec.Gates = nil
	spec.Tasks = []apiv1.Task{
		{Name: "select", Type: apiv1.TaskDeterministic, Goal: "select revision", Run: &apiv1.DeterministicRun{Command: []string{"true"}}, Next: "inspect"},
		{Name: "inspect", Type: apiv1.TaskDeterministic, Goal: "inspect revision", Run: &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceRepoReadOnly}, Next: workflow.TerminalComplete},
	}
	machine := compileCredentialPlaneMachine(t, spec)
	service, scrubber, runID := newCredentialPlaneFixture(t, machine)
	base := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"}
	source := base
	if fork {
		source.Owner = "contributor"
	}
	scope := credentialGaggleScope{Project: base}
	if fork {
		scope.AdditionalRepos = []apiv1.RepoRef{source}
	}
	service.Replace(credentialPlaneDefinitions{Scopes: map[string]credentialGaggleScope{"web": scope}})
	t.Setenv("REVISION_BASE_TOKEN", "revision-base-secret-123456789")
	t.Setenv("REVISION_FORK_TOKEN", "revision-fork-secret-123456789")
	service.config = &instance.Config{Repos: []instance.RepoRef{
		{Provider: "github", Owner: base.Owner, Name: base.Name, Token: instance.TokenRef{Env: "REVISION_BASE_TOKEN"}},
		{Provider: "github", Owner: "contributor", Name: source.Name, Token: instance.TokenRef{Env: "REVISION_FORK_TOKEN"}},
	}}
	service.buildSources = nil
	revision := &apiv1.WorkspaceRevision{
		Repository: apiv1.RepositoryIdentity{
			Provider: source.Provider, Owner: source.Owner, Name: source.Name,
			URL: "https://github.com/" + source.Owner + "/" + source.Name,
		},
		CommitSHA: strings.Repeat("a", 40), SourceRef: "refs/heads/topic", SourceID: "17",
	}
	if accepted {
		run, _, err := journal.Recover(filepath.Join(service.layout.ForGaggle("web").RunsDir(), runID))
		if err != nil {
			t.Fatal(err)
		}
		if err := run.Append(journal.Event{Type: journal.EventStageFinished, Stage: "select", Status: "success", WorkspaceRevision: revision}); err != nil {
			_ = run.Close()
			t.Fatal(err)
		}
		if err := run.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return service, scrubber, httpapi.CredentialResolveRequest{RunID: runID, Stage: "inspect", WorkspaceRevision: revision}
}

func TestCredentialPlaneRevisionCheckoutUsesOnlySourceReadGrant(t *testing.T) {
	for _, fork := range []bool{false, true} {
		name := "base"
		want := "revision-base-secret-123456789"
		if fork {
			name, want = "fork", "revision-fork-secret-123456789"
		}
		t.Run(name, func(t *testing.T) {
			service, scrubber, request := revisionCredentialFixture(t, fork, true)
			response, err := service.Resolve(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			if len(response.Credentials) != 1 || response.Credentials[0].Capability != httpapi.WorkspaceRevisionCheckoutCapability || response.Credentials[0].Value != want {
				t.Fatalf("incorrect checkout credential identity: %+v", response)
			}
			if string(scrubber.Scrub([]byte(want))) == want {
				t.Fatal("checkout credential was not registered with the scrubber")
			}
			if fork && string(scrubber.Scrub([]byte("revision-base-secret-123456789"))) != "revision-base-secret-123456789" {
				t.Fatal("fork checkout materialized the base credential")
			}
		})
	}
}

func TestCredentialPlaneRevisionCheckoutRefusesUnboundAuthority(t *testing.T) {
	for _, name := range []string{"unaccepted", "different-sha", "different-source", "undeclared-source", "wrong-stage", "mixed-capabilities", "missing-token", "missing-read-grant"} {
		t.Run(name, func(t *testing.T) {
			service, scrubber, request := revisionCredentialFixture(t, true, name != "unaccepted")
			wantCode := "workspace_revision_unauthorized"
			switch name {
			case "different-sha":
				request.WorkspaceRevision.CommitSHA = strings.Repeat("b", 40)
				wantCode = "workspace_revision_conflict"
			case "different-source":
				request.WorkspaceRevision.Repository.Owner = "other"
				request.WorkspaceRevision.Repository.URL = "https://github.com/other/web"
				wantCode = "workspace_revision_conflict"
			case "undeclared-source":
				defs := *service.defs.Load()
				defs.Scopes = map[string]credentialGaggleScope{"web": {Project: defs.Scopes["web"].Project}}
				service.Replace(defs)
			case "wrong-stage":
				request.Stage = "select"
			case "mixed-capabilities":
				request.Capabilities = []string{"repo:push"}
				wantCode = "capability_undeclared"
			case "missing-token":
				t.Setenv("REVISION_FORK_TOKEN", "")
				wantCode = "credential_resolution_failed"
			case "missing-read-grant":
				service.buildSources = func(scope credentialGaggleScope) (credentials.Resolver, []credentials.Grant, error) {
					return buildCredentials(service.config, nil, scope.Project.Owner, scope.Project.Name, nil, scrubber)
				}
			}
			_, err := service.Resolve(context.Background(), request)
			if err == nil {
				t.Fatal("unauthorized or unavailable checkout unexpectedly succeeded")
			}
			if got := planeErrorOf(t, err).Code; got != wantCode {
				t.Fatalf("code = %s, want %s", got, wantCode)
			}
			if string(scrubber.Scrub([]byte("revision-base-secret-123456789"))) != "revision-base-secret-123456789" {
				t.Fatal("refused checkout fell back to the base credential")
			}
		})
	}
}

func TestCredentialPlaneRevisionCheckoutAllowsExplicitPublicSource(t *testing.T) {
	service, _, request := revisionCredentialFixture(t, true, true)
	service.config.Repos[1].Token = instance.TokenRef{}
	response, err := service.Resolve(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Credentials) != 0 {
		t.Fatalf("public source inherited a credential: %+v", response)
	}
}
