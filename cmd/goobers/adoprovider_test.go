package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

// deliveredADOStageCapabilities are the repository capabilities shipped Azure
// DevOps stages declare. setDeliveredADOStageCredentials delivers each one.
var deliveredADOStageCapabilities = []string{
	"github:issues:read", "github:issues:write", "github:pr:write", "github:branch:delete", "provider:pr:write", "repo:push",
}

// deliveredADOStageToken is the distinct value setDeliveredADOStageCredentials
// delivers for capability, so a test can tell which capability's credential a
// provider was built from.
func deliveredADOStageToken(capability string) string {
	return "delivered-" + strings.ReplaceAll(capability, ":", "-")
}

// setDeliveredADOStageCredentials stamps what the daemon delivers to an Azure
// DevOps stage under Microsoft Entra auth: one GOOBERS_CRED_<capability> value
// per declared repository capability, and the bearer scheme beside them. It
// leaves ado:pr:complete to the tests that exercise completion authority.
func setDeliveredADOStageCredentials(t *testing.T) {
	t.Helper()
	for _, capability := range deliveredADOStageCapabilities {
		t.Setenv(executor.CredentialEnvVar(capability), deliveredADOStageToken(capability))
	}
	t.Setenv(executor.RepoAuthSchemeEnvVar, "bearer")
}

// adoWorkItemServer answers the two calls GetWorkItem makes and records the
// Authorization header of every request.
func adoWorkItemServer(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var headers []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers = append(headers, r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/org/project/_apis/wit/workitems/42":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": 42,
				"fields": map[string]any{
					"System.WorkItemType": "Task",
					"System.Title":        "Backlog item",
					"System.State":        "Active",
				},
			})
		case "/org/project/_apis/wit/workitemtypes/Task/states":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"value": []map[string]string{{"name": "Active", "category": "InProgress"}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server, &headers
}

// adoConfiguredPATFixture writes an instance whose only repository is an ADO
// repo authenticated by a configured PAT, and sets that PAT. A stage must not
// use it: it authenticates with its delivered capability credential instead.
func adoConfiguredPATFixture(t *testing.T) (string, providers.RepositoryRef) {
	t.Helper()
	root := initDemo(t)
	cfg, err := instance.LoadConfig(layoutFor(root).ConfigFile())
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.Repos = []instance.RepoRef{{
		Provider: "ado",
		Owner:    "org",
		Project:  "project",
		Name:     "repo",
		Token:    instance.TokenRef{Env: "ADO_CONFIGURED_PAT"},
	}}
	if err := instance.WriteConfig(layoutFor(root).ConfigFile(), cfg); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("ADO_CONFIGURED_PAT", "configured-pat")
	return root, providers.RepositoryRef{Provider: providers.ProviderADO, Owner: "org", Project: "project", Name: "repo"}
}

// pointADOStageProviderAt keeps the real stage constructor (so the delivered
// credential source is exactly what production builds) and only redirects the
// provider at server.
func pointADOStageProviderAt(t *testing.T, server *httptest.Server) {
	t.Helper()
	previous := newADOProviderForStage
	newADOProviderForStage = func(routed providers.RepositoryRef, credential providers.ADOCredentialSource) (*providers.ADOProvider, error) {
		provider, err := buildADOProviderForStage(routed, credential)
		if err != nil {
			return nil, err
		}
		provider.BaseURL = server.URL
		return provider, nil
	}
	t.Cleanup(func() { newADOProviderForStage = previous })
}

// TestADOStageProviderUsesTheDeclaredCapabilityCredential is the ADO-N18
// factory contract: a stage's ADO provider authenticates with the value
// delivered for the capability it declared, in the delivered scheme, and never
// with another capability's value or the repository's configured PAT.
func TestADOStageProviderUsesTheDeclaredCapabilityCredential(t *testing.T) {
	basic := func(secret string) string {
		return "Basic " + base64.StdEncoding.EncodeToString([]byte("goobers:"+secret))
	}
	for _, tc := range []struct {
		name   string
		scheme string
		want   string
	}{
		{name: "bearer", scheme: "bearer", want: "Bearer issues-write-token"},
		{name: "basic", scheme: "basic", want: basic("issues-write-token")},
		{name: "no scheme defaults to basic", want: basic("issues-write-token")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, repo := adoConfiguredPATFixture(t)
			server, headers := adoWorkItemServer(t)
			pointADOStageProviderAt(t, server)
			t.Setenv(executor.CredentialEnvVar("github:issues:write"), "issues-write-token")
			t.Setenv(executor.CredentialEnvVar("provider:pr:write"), "wrong-pr-token")
			t.Setenv(executor.RepoAuthSchemeEnvVar, tc.scheme)

			provider, err := newProviderForStage(root, repo, false)
			if err != nil {
				t.Fatalf("build ADO stage provider: %v", err)
			}
			if _, err := provider.GetWorkItem(context.Background(), repo, "42"); err != nil {
				t.Fatalf("get backlog work item: %v", err)
			}
			for _, got := range *headers {
				if got != tc.want {
					t.Fatalf("Authorization = %q, want %q (the declared capability's delivered credential)", got, tc.want)
				}
			}
			if len(*headers) == 0 {
				t.Fatal("no request reached the server")
			}
		})
	}
}

// TestADOStageProviderWithoutDeclaredCapabilityHasNoCredential pins "an
// undeclared capability means no credential" on Azure DevOps: with the
// repository's PAT configured and set, a stage whose capability delivered
// nothing still fails, naming the variable it needed.
func TestADOStageProviderWithoutDeclaredCapabilityHasNoCredential(t *testing.T) {
	root, repo := adoConfiguredPATFixture(t)
	t.Setenv(executor.CredentialEnvVar("github:issues:write"), "")
	t.Setenv(executor.CredentialEnvVar("provider:pr:write"), "other-capability-token")
	_, err := newProviderForStage(root, repo, false)
	if err == nil || !strings.Contains(err.Error(), "GOOBERS_CRED_GITHUB_ISSUES_WRITE") {
		t.Fatalf("error = %v, want a missing GOOBERS_CRED_GITHUB_ISSUES_WRITE failure", err)
	}
}

// TestADOOperatorProviderKeepsConfiguredAuth pins the one exception: an
// operator command that is not a stage (goobers run, goobers status) opts into
// the repository's configured auth and keeps working as before.
func TestADOOperatorProviderKeepsConfiguredAuth(t *testing.T) {
	root, repo := adoConfiguredPATFixture(t)
	server, headers := adoWorkItemServer(t)
	t.Setenv(executor.CredentialEnvVar("github:issues:read"), "")

	provider, err := newProviderForStage(root, repo, true, withStageProviderConfiguredADOAuth())
	if err != nil {
		t.Fatalf("build ADO operator provider: %v", err)
	}
	ado, ok := provider.(*providers.ADOProvider)
	if !ok {
		t.Fatalf("provider = %T, want *providers.ADOProvider", provider)
	}
	ado.BaseURL = server.URL
	if _, err := ado.GetWorkItem(context.Background(), repo, "42"); err != nil {
		t.Fatalf("get backlog work item: %v", err)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("goobers:configured-pat"))
	if len(*headers) == 0 || (*headers)[0] != want {
		t.Fatalf("Authorization = %q, want the configured PAT %q", *headers, want)
	}
}
