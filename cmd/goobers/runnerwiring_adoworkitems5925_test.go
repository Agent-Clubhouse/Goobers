package main

import (
	"context"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/adoauth"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

// #5925: an Azure DevOps repository's own credential backs
// ado:work-items:write, as it backs provider:pr:write and ado:pr:complete. A
// GitHub or Gitea repository credential never backs it.

func grantRefFor(grants []credentials.Grant, c capability.Capability) (string, bool) {
	for _, grant := range grants {
		if grant.Capability == string(c) {
			return grant.Ref, true
		}
	}
	return "", false
}

func mixedProviderCredentialConfig() *instance.Config {
	return &instance.Config{Repos: []instance.RepoRef{
		{Provider: "github", Owner: "example-gh", Name: "gh-repo", Token: instance.TokenRef{Env: "EXAMPLE_GH_TOKEN"}},
		{Provider: "gitea", Owner: "example-gitea", Name: "gitea-repo", Token: instance.TokenRef{Env: "EXAMPLE_GITEA_TOKEN"}},
		adoTestRepo(nil, instance.TokenRef{Env: "EXAMPLE_ADO_PAT"}),
	}}
}

func TestBuildCredentialsBacksADOWorkItemsWriteFromTheADORepoOnly(t *testing.T) {
	t.Setenv("EXAMPLE_GH_TOKEN", "gh-token")
	t.Setenv("EXAMPLE_GITEA_TOKEN", "gitea-token")
	t.Setenv("EXAMPLE_ADO_PAT", "ado-pat")
	cfg := mixedProviderCredentialConfig()

	_, adoGrants, err := buildCredentials(cfg, nil, adoTestOwner, adoTestName, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ref, ok := grantRefFor(adoGrants, capability.ADOWorkItemsWrite); !ok || ref != adoTestRef {
		t.Fatalf("ADO gaggle %s grant ref = %q (granted %t), want the ADO repository's own %q", capability.ADOWorkItemsWrite, ref, ok, adoTestRef)
	}

	for _, repo := range []struct{ owner, name string }{{"example-gh", "gh-repo"}, {"example-gitea", "gitea-repo"}} {
		_, grants, err := buildCredentials(cfg, nil, repo.owner, repo.name, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if ref, ok := grantRefFor(grants, capability.ADOWorkItemsWrite); ok {
			t.Fatalf("%s/%s gaggle was granted %s from %q; a non-ADO repository credential must not back it", repo.owner, repo.name, capability.ADOWorkItemsWrite, ref)
		}
		// Every other repository-backed capability is unchanged.
		for _, c := range credentialedCapabilities {
			if ref, ok := grantRefFor(grants, c); !ok || ref != repo.owner+"/"+repo.name {
				t.Fatalf("%s/%s gaggle %s grant ref = %q (granted %t), want its own repository", repo.owner, repo.name, c, ref, ok)
			}
		}
	}
}

func TestBuildCredentialsKeepsAnExplicitADOWorkItemsWriteEntry(t *testing.T) {
	t.Setenv("EXAMPLE_GH_TOKEN", "gh-token")
	t.Setenv("EXAMPLE_GITEA_TOKEN", "gitea-token")
	t.Setenv("EXAMPLE_ADO_PAT", "ado-pat")
	t.Setenv("EXAMPLE_WORK_ITEMS_PAT", "work-items-pat")
	cfg := mixedProviderCredentialConfig()
	cfg.Credentials = []instance.CredentialGrant{{
		Capability: string(capability.ADOWorkItemsWrite),
		Token:      instance.TokenRef{Env: "EXAMPLE_WORK_ITEMS_PAT"},
	}}
	for _, repo := range []struct{ owner, name string }{{adoTestOwner, adoTestName}, {"example-gh", "gh-repo"}} {
		_, grants, err := buildCredentials(cfg, nil, repo.owner, repo.name, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		want := credentialRefName(string(capability.ADOWorkItemsWrite))
		if ref, ok := grantRefFor(grants, capability.ADOWorkItemsWrite); !ok || ref != want {
			t.Fatalf("%s/%s %s grant ref = %q (granted %t), want the explicit entry %q", repo.owner, repo.name, capability.ADOWorkItemsWrite, ref, ok, want)
		}
	}
}

func TestConfiguredCredentialGrantsBackADOWorkItemsWriteOnADOOnly(t *testing.T) {
	cfg := mixedProviderCredentialConfig()
	ado, err := configuredCredentialGrants(cfg, apiv1.RepoRef{Provider: apiv1.ProviderADO, Owner: "example-org", Project: "example-project", Name: adoTestName}, apiv1.BacklogRef{})
	if err != nil {
		t.Fatal(err)
	}
	if !ado[string(capability.ADOWorkItemsWrite)] {
		t.Fatalf("ADO project grants = %v, want %s backed by the repository credential", ado, capability.ADOWorkItemsWrite)
	}
	gh, err := configuredCredentialGrants(cfg, apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "example-gh", Name: "gh-repo"}, apiv1.BacklogRef{})
	if err != nil {
		t.Fatal(err)
	}
	if gh[string(capability.ADOWorkItemsWrite)] {
		t.Fatalf("GitHub project grants = %v, want no %s", gh, capability.ADOWorkItemsWrite)
	}
	if !gh[string(capability.ProviderPRWrite)] {
		t.Fatalf("GitHub project grants = %v, want %s unchanged", gh, capability.ProviderPRWrite)
	}
}

// TestCredentialPlaneResolvesADOWorkItemsWriteFromTheRepoSource is the pod
// half: a stage declaring ado:work-items:write on an azure-cli ADO gaggle
// resolves it from the repository's own minted credential.
func TestCredentialPlaneResolvesADOWorkItemsWriteFromTheRepoSource(t *testing.T) {
	expires := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	azure := &adoTestAzureRunner{token: "entra-work-items-token-0123456789", expires: expires}
	stubADOCredentialSource(t, func(repo instance.RepoRef, stores credentials.StoreResolver) (providers.ADOCredentialSource, error) {
		return adoauth.Source(repo, azure, stores)
	})
	spec := credentialPlaneSpec()
	spec.Tasks[1].Capabilities = []string{string(capability.ADOWorkItemsWrite)}
	machine := compileCredentialPlaneMachine(t, spec)
	service, _, runID := newCredentialPlaneFixture(t, machine)
	service.buildSources = nil // the daemon's own buildCredentials
	service.config = &instance.Config{Repos: []instance.RepoRef{
		adoTestRepo(&instance.RepoAuthConfig{Kind: instance.ADOAuthAzureCLI}, instance.TokenRef{}),
	}}
	service.Replace(credentialPlaneDefinitions{
		Scopes: map[string]credentialGaggleScope{"web": {Project: apiv1.RepoRef{
			Provider: apiv1.ProviderADO, Owner: "example-org", Project: "example-project", Name: adoTestName,
		}}},
		Goobers: credentialPlaneGoobers(),
	})

	response, err := service.Resolve(context.Background(), httpapi.CredentialResolveRequest{RunID: runID, Stage: "push-branch"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(response.Credentials) != 1 || response.Credentials[0].Capability != string(capability.ADOWorkItemsWrite) {
		t.Fatalf("credentials = %+v, want %s", response.Credentials, capability.ADOWorkItemsWrite)
	}
	if response.Credentials[0].Value != azure.token {
		t.Fatalf("%s value was not the repository's minted token", capability.ADOWorkItemsWrite)
	}
}
