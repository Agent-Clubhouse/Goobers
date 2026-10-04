package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/adoauth"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

// #5925/#6581: an Azure DevOps repository's own credential backs
// ado:work-items:write and ado:packaging:read, as it backs provider:pr:write
// and ado:pr:complete. A GitHub or Gitea repository credential never backs
// them.

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

func TestBuildCredentialsBacksADOOnlyCapabilitiesFromTheADORepoOnly(t *testing.T) {
	t.Setenv("EXAMPLE_GH_TOKEN", "gh-token")
	t.Setenv("EXAMPLE_GITEA_TOKEN", "gitea-token")
	t.Setenv("EXAMPLE_ADO_PAT", "ado-pat")
	cfg := mixedProviderCredentialConfig()

	_, adoGrants, err := buildCredentials(cfg, nil, adoTestOwner, adoTestName, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range adoRepoCredentialedCapabilities {
		if ref, ok := grantRefFor(adoGrants, c); !ok || ref != adoTestRef {
			t.Fatalf("ADO gaggle %s grant ref = %q (granted %t), want the ADO repository's own %q", c, ref, ok, adoTestRef)
		}
	}

	for _, repo := range []struct{ owner, name string }{{"example-gh", "gh-repo"}, {"example-gitea", "gitea-repo"}} {
		_, grants, err := buildCredentials(cfg, nil, repo.owner, repo.name, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range adoRepoCredentialedCapabilities {
			if ref, ok := grantRefFor(grants, c); ok {
				t.Fatalf("%s/%s gaggle was granted %s from %q; a non-ADO repository credential must not back it", repo.owner, repo.name, c, ref)
			}
		}
		// Every other repository-backed capability is unchanged.
		for _, c := range credentialedCapabilities {
			if ref, ok := grantRefFor(grants, c); !ok || ref != repo.owner+"/"+repo.name {
				t.Fatalf("%s/%s gaggle %s grant ref = %q (granted %t), want its own repository", repo.owner, repo.name, c, ref, ok)
			}
		}
	}
}

func TestBuildCredentialsKeepsExplicitADOOnlyCapabilityEntries(t *testing.T) {
	t.Setenv("EXAMPLE_GH_TOKEN", "gh-token")
	t.Setenv("EXAMPLE_GITEA_TOKEN", "gitea-token")
	t.Setenv("EXAMPLE_ADO_PAT", "ado-pat")
	t.Setenv("EXAMPLE_ADO_ONLY_PAT", "ado-only-pat")
	cfg := mixedProviderCredentialConfig()
	for _, c := range adoRepoCredentialedCapabilities {
		cfg.Credentials = []instance.CredentialGrant{{
			Capability: string(c),
			Token:      instance.TokenRef{Env: "EXAMPLE_ADO_ONLY_PAT"},
		}}
		for _, repo := range []struct{ owner, name string }{{adoTestOwner, adoTestName}, {"example-gh", "gh-repo"}} {
			_, grants, err := buildCredentials(cfg, nil, repo.owner, repo.name, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			want := credentialRefName(string(c))
			if ref, ok := grantRefFor(grants, c); !ok || ref != want {
				t.Fatalf("%s/%s %s grant ref = %q (granted %t), want the explicit entry %q", repo.owner, repo.name, c, ref, ok, want)
			}
		}
	}
}

func TestConfiguredCredentialGrantsBackADOOnlyCapabilitiesOnADOOnly(t *testing.T) {
	cfg := mixedProviderCredentialConfig()
	ado, err := configuredCredentialGrants(cfg, apiv1.RepoRef{Provider: apiv1.ProviderADO, Owner: "example-org", Project: "example-project", Name: adoTestName}, apiv1.BacklogRef{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range adoRepoCredentialedCapabilities {
		if !ado[string(c)] {
			t.Fatalf("ADO project grants = %v, want %s backed by the repository credential", ado, c)
		}
	}
	gh, err := configuredCredentialGrants(cfg, apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "example-gh", Name: "gh-repo"}, apiv1.BacklogRef{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range adoRepoCredentialedCapabilities {
		if gh[string(c)] {
			t.Fatalf("GitHub project grants = %v, want no %s", gh, c)
		}
	}
	if !gh[string(capability.ProviderPRWrite)] {
		t.Fatalf("GitHub project grants = %v, want %s unchanged", gh, capability.ProviderPRWrite)
	}
}

// TestCredentialPlaneResolvesADOOnlyCapabilitiesFromTheRepoSource is the pod
// half: a stage declaring ADO-only repo-backed capabilities on an azure-cli
// ADO gaggle resolves them from the repository's own minted credential.
func TestCredentialPlaneResolvesADOOnlyCapabilitiesFromTheRepoSource(t *testing.T) {
	expires := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	azure := &adoTestAzureRunner{token: "entra-ado-only-token-0123456789", expires: expires}
	stubADOCredentialSource(t, func(repo instance.RepoRef, stores credentials.StoreResolver) (providers.ADOCredentialSource, error) {
		return adoauth.Source(repo, azure, stores)
	})
	spec := credentialPlaneSpec()
	spec.Tasks[1].Capabilities = []string{
		string(capability.ADOWorkItemsWrite),
		string(capability.ADOPackagingRead),
	}
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
	if len(response.Credentials) != 2 {
		t.Fatalf("credentials = %+v, want two ADO-only capabilities", response.Credentials)
	}
	byCapability := map[string]httpapi.MintedCredential{}
	for _, credential := range response.Credentials {
		byCapability[credential.Capability] = credential
	}
	for _, c := range []capability.Capability{capability.ADOWorkItemsWrite, capability.ADOPackagingRead} {
		credential, ok := byCapability[string(c)]
		if !ok {
			t.Fatalf("credentials = %+v, missing %s", response.Credentials, c)
		}
		if credential.Value != azure.token {
			t.Fatalf("%s value was not the repository's minted token", c)
		}
		if credential.ExpiresAt == nil || !credential.ExpiresAt.Equal(expires) {
			t.Fatalf("%s expiry = %v, want %v", c, credential.ExpiresAt, expires)
		}
	}
	if response.RepoAuthScheme != "bearer" {
		t.Fatalf("repoAuthScheme = %q, want bearer", response.RepoAuthScheme)
	}

	data, err := os.ReadFile(filepath.Join(service.layout.SchedulerDir(), "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), azure.token) {
		t.Fatal("credential audit contains the minted value")
	}
	var found bool
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var event journal.Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("parse instance event %q: %v", line, err)
		}
		if event.Type != journal.EventRunnerAnnotation || event.Runner["kind"] != credentialResolutionMarker {
			continue
		}
		materialized, _ := event.Runner["materialized"].([]any)
		var names []string
		for _, name := range materialized {
			names = append(names, name.(string))
		}
		joined := strings.Join(names, ",")
		if strings.Contains(joined, string(capability.ADOWorkItemsWrite)) && strings.Contains(joined, string(capability.ADOPackagingRead)) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("credential audit did not name both ADO-only capabilities:\n%s", data)
	}
}
