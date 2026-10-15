package main

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/adoauth"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

// adoEntraGaggleWithPATOverride is the configuration #5957 reported: an Azure
// DevOps repository authenticated as azure-cli (Bearer), plus a credentials:
// entry that sources one capability from a personal access token.
func adoEntraGaggleWithPATOverride(capabilityName string) *instance.Config {
	return &instance.Config{
		Repos: []instance.RepoRef{adoTestRepo(&instance.RepoAuthConfig{Kind: instance.ADOAuthAzureCLI}, instance.TokenRef{})},
		Credentials: []instance.CredentialGrant{{
			Capability: capabilityName,
			Token:      instance.TokenRef{Env: "EXAMPLE_OVERRIDE_PAT"},
		}},
	}
}

var adoTestProject = apiv1.RepoRef{Provider: apiv1.ProviderADO, Owner: "example-org", Project: "example-project", Name: adoTestName}

// TestADOCredentialSchemesFollowEachCredentialsSource is the rule: a
// capability backed by the repository's own credential keeps the repository's
// scheme, one backed by a static credentials: entry is "basic", and a gaggle
// whose repository is not on Azure DevOps states none.
func TestADOCredentialSchemesFollowEachCredentialsSource(t *testing.T) {
	cfg := adoEntraGaggleWithPATOverride(string(capability.ADOWorkItemsWrite))
	grants := []credentials.Grant{
		{Capability: string(capability.RepoPush), Ref: adoTestRef},
		{Capability: string(capability.ADOWorkItemsWrite), Ref: credentialRefName(string(capability.ADOWorkItemsWrite))},
		{Capability: string(capability.GitHubIssuesWrite), Ref: daemonIdentityRefName},
	}
	got := adoCredentialSchemes(cfg, adoTestProject, grants)
	want := map[string]string{
		string(capability.RepoPush):          adoauth.SchemeBearer,
		string(capability.ADOWorkItemsWrite): adoauth.SchemeBasic,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("schemes = %v, want %v (no daemon identity is configured)", got, want)
	}

	// A static daemon identity on an ADO gaggle is a PAT: basic, even on an
	// Entra repository. A GitHub App daemon identity states none.
	cfg.DaemonIdentity = &instance.DaemonIdentityConfig{Kind: instance.GitHubAuthPAT, Token: &instance.TokenRef{Env: "EXAMPLE_DAEMON_PAT"}}
	if got := adoCredentialSchemes(cfg, adoTestProject, grants)[string(capability.GitHubIssuesWrite)]; got != adoauth.SchemeBasic {
		t.Fatalf("static daemon identity scheme = %q, want basic", got)
	}

	patRepo := &instance.Config{Repos: []instance.RepoRef{adoTestRepo(&instance.RepoAuthConfig{Kind: instance.ADOAuthPAT}, instance.TokenRef{Env: "EXAMPLE_ADO_PAT"})}}
	if got := adoCredentialSchemes(patRepo, adoTestProject, grants[:1]); got[string(capability.RepoPush)] != adoauth.SchemeBasic {
		t.Fatalf("PAT repository schemes = %v, want repo:push basic", got)
	}

	github := &instance.Config{Repos: []instance.RepoRef{{Provider: "github", Owner: "acme", Name: "app"}}}
	if got := adoCredentialSchemes(github, apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "app"}, grants); got != nil {
		t.Fatalf("GitHub gaggle schemes = %v, want none", got)
	}
}

// TestCredentialPlaneStatesBasicForACredentialsPATOnAnEntraGaggle is #5957's
// pod half: on an azure-cli gaggle, a capability sourced from a credentials:
// PAT is minted with scheme "basic", while the response still states the
// repository's "bearer", and the pod hands the stage both.
func TestCredentialPlaneStatesBasicForACredentialsPATOnAnEntraGaggle(t *testing.T) {
	const pat = "override-pat-value-0123456789"
	t.Setenv("EXAMPLE_OVERRIDE_PAT", pat)
	azure := &adoTestAzureRunner{token: "entra-plane-token-0123456789", expires: time.Now().Add(time.Hour)}
	stubADOCredentialSource(t, func(repo instance.RepoRef, stores credentials.StoreResolver) (providers.ADOCredentialSource, error) {
		return adoauth.Source(repo, azure, stores)
	})
	machine := compileCredentialPlaneMachine(t, credentialPlaneSpec())
	service, shared, runID := newCredentialPlaneFixture(t, machine)
	service.buildSources = nil // the daemon's own buildCredentials
	service.config = adoEntraGaggleWithPATOverride(string(capability.RepoPush))
	service.Replace(credentialPlaneDefinitions{
		Scopes:  map[string]credentialGaggleScope{"web": {Project: adoTestProject}},
		Goobers: credentialPlaneGoobers(),
	})

	response, err := service.Resolve(context.Background(), httpapi.CredentialResolveRequest{RunID: runID, Stage: "push-branch"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(response.Credentials) != 1 || response.Credentials[0].Value != pat {
		t.Fatalf("credentials = %d, want repo:push from the credentials: PAT", len(response.Credentials))
	}
	if got := response.Credentials[0].Scheme; got != adoauth.SchemeBasic {
		t.Fatalf("repo:push scheme = %q, want %q for a credentials: PAT", got, adoauth.SchemeBasic)
	}
	if response.RepoAuthScheme != adoauth.SchemeBearer {
		t.Fatalf("repoAuthScheme = %q, want the repository's %q", response.RepoAuthScheme, adoauth.SchemeBearer)
	}
	if strings.Contains(string(shared.Scrub([]byte(pat))), pat) {
		t.Fatal("the credentials: PAT was not registered with the shared scrubber")
	}

	minted := []dispatcher.MintedCredential{{Capability: response.Credentials[0].Capability, Value: response.Credentials[0].Value, Scheme: response.Credentials[0].Scheme}}
	env := stageCredentialEnv(minted, response.RepoAuthScheme)
	if !containsEnvEntry(env, executor.CredentialSchemeEnvVar(string(capability.RepoPush))+"=basic") {
		t.Fatalf("pod stage env = %q, want repo:push stated as basic", env)
	}

	// The pod's scrubber redacts the form the PAT is actually sent in.
	basicForm := base64.StdEncoding.EncodeToString([]byte("goobers:" + pat))
	if got := string(podStageScrubber(minted, response.RepoAuthScheme).Scrub([]byte("Authorization: Basic " + basicForm))); strings.Contains(got, basicForm) {
		t.Fatalf("pod scrubber left the PAT's Basic form: %q", got)
	}
}

func containsEnvEntry(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// TestCredentialPlaneStatesBasicForADaemonIdentityPATOnAnEntraGaggle is the
// daemon-identity sibling of the credentials: case, end to end through the
// daemon's own buildCredentials: on an azure-cli gaggle whose daemonIdentity
// is a PAT, repo:push resolves to that PAT and is stated "basic", while the
// response still states the repository's "bearer".
func TestCredentialPlaneStatesBasicForADaemonIdentityPATOnAnEntraGaggle(t *testing.T) {
	const pat = "daemon-identity-pat-0123456789"
	t.Setenv("EXAMPLE_DAEMON_PAT", pat)
	azure := &adoTestAzureRunner{token: "entra-plane-token-0123456789", expires: time.Now().Add(time.Hour)}
	stubADOCredentialSource(t, func(repo instance.RepoRef, stores credentials.StoreResolver) (providers.ADOCredentialSource, error) {
		return adoauth.Source(repo, azure, stores)
	})
	machine := compileCredentialPlaneMachine(t, credentialPlaneSpec())
	service, _, runID := newCredentialPlaneFixture(t, machine)
	service.buildSources = nil // the daemon's own buildCredentials
	service.config = &instance.Config{
		Repos:          []instance.RepoRef{adoTestRepo(&instance.RepoAuthConfig{Kind: instance.ADOAuthAzureCLI}, instance.TokenRef{})},
		DaemonIdentity: &instance.DaemonIdentityConfig{Kind: instance.GitHubAuthPAT, Token: &instance.TokenRef{Env: "EXAMPLE_DAEMON_PAT"}},
	}
	service.Replace(credentialPlaneDefinitions{
		Scopes:  map[string]credentialGaggleScope{"web": {Project: adoTestProject}},
		Goobers: credentialPlaneGoobers(),
	})

	response, err := service.Resolve(context.Background(), httpapi.CredentialResolveRequest{RunID: runID, Stage: "push-branch"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(response.Credentials) != 1 || response.Credentials[0].Value != pat {
		t.Fatalf("credentials = %d, want repo:push from the daemon identity PAT", len(response.Credentials))
	}
	if got := response.Credentials[0].Scheme; got != adoauth.SchemeBasic {
		t.Fatalf("repo:push scheme = %q, want %q for a daemon identity PAT", got, adoauth.SchemeBasic)
	}
	if response.RepoAuthScheme != adoauth.SchemeBearer {
		t.Fatalf("repoAuthScheme = %q, want the repository's %q", response.RepoAuthScheme, adoauth.SchemeBearer)
	}
}

// TestADOStageProviderSendsAPerCapabilitySchemeOverTheRepository is #5957's
// stage half: GOOBERS_CREDENTIAL_SCHEME_<cap> wins over
// GOOBERS_REPO_AUTH_SCHEME, so a credentials: PAT on an Entra gaggle is sent
// as Basic instead of the Bearer that earned a 401; a capability with no
// scheme of its own keeps the repository's.
func TestADOStageProviderSendsAPerCapabilitySchemeOverTheRepository(t *testing.T) {
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("goobers:issues-write-pat"))
	for _, tc := range []struct {
		name      string
		capScheme string
		want      string
	}{
		{name: "per-capability basic over a bearer repository", capScheme: "basic", want: basic},
		{name: "no per-capability scheme keeps the repository's", want: "Bearer issues-write-pat"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, repo := adoConfiguredPATFixture(t)
			server, headers := adoWorkItemServer(t)
			pointADOStageProviderAt(t, server)
			t.Setenv(executor.CredentialEnvVar("github:issues:write"), "issues-write-pat")
			t.Setenv(executor.CredentialSchemeEnvVar("github:issues:write"), tc.capScheme)
			t.Setenv(executor.RepoAuthSchemeEnvVar, "bearer")

			provider, err := newProviderForStage(root, repo, false)
			if err != nil {
				t.Fatalf("build ADO stage provider: %v", err)
			}
			if _, err := provider.GetWorkItem(context.Background(), repo, "42"); err != nil {
				t.Fatalf("get backlog work item: %v", err)
			}
			if len(*headers) == 0 {
				t.Fatal("no request reached the server")
			}
			for _, got := range *headers {
				if got != tc.want {
					t.Fatalf("Authorization = %q, want %q", got, tc.want)
				}
			}
		})
	}
}

// TestStageADOCredentialKindNamesTheVariableItRead: an unsupported scheme
// fails closed and names the variable that carried it, per capability or
// repository-wide.
func TestStageADOCredentialKindNamesTheVariableItRead(t *testing.T) {
	cap := capability.GitHubPRWrite
	t.Setenv(executor.RepoAuthSchemeEnvVar, "bearer")
	t.Setenv(executor.CredentialSchemeEnvVar(string(cap)), "digest")
	if _, err := stageADOCredentialKind(cap); err == nil || !strings.Contains(err.Error(), executor.CredentialSchemeEnvVar(string(cap))) {
		t.Fatalf("error = %v, want it to name %s", err, executor.CredentialSchemeEnvVar(string(cap)))
	}
	t.Setenv(executor.CredentialSchemeEnvVar(string(cap)), "")
	t.Setenv(executor.RepoAuthSchemeEnvVar, "digest")
	if _, err := stageADOCredentialKind(cap); err == nil || !strings.Contains(err.Error(), executor.RepoAuthSchemeEnvVar) {
		t.Fatalf("error = %v, want it to name %s", err, executor.RepoAuthSchemeEnvVar)
	}
}

// TestBuildTerminalBranchDeleteSendsACredentialsPATAsBasic: the terminal
// arm states github:branch:delete's own scheme, so a credentials: PAT on an
// azure-cli repository is sent as Basic.
func TestBuildTerminalBranchDeleteSendsACredentialsPATAsBasic(t *testing.T) {
	const branch = "goobers/implementation/terminal-run"
	const pat = "terminal-override-pat"
	t.Setenv("EXAMPLE_OVERRIDE_PAT", pat)
	refs := newFakeADORefs(map[string]string{branch: "7777777777777777777777777777777777777777"})
	mux := http.NewServeMux()
	refs.register(t, mux, "example-org", "example-project", "example-repo")
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	pointTerminalADODeleterAt(t, server.URL)

	cfg := adoEntraGaggleWithPATOverride(string(capability.GitHubBranchDelete))
	deleteBranch, repo, err := buildTerminalBranchDelete(cfg, apiv1.RepoRef{}, journal.NewRegistryScrubber(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deleteBranch(context.Background(), providers.DeleteBranchRequest{Repository: repo, Name: branch}); err != nil {
		t.Fatal(err)
	}
	_, updates, auths := refs.snapshot()
	if len(updates) != 1 {
		t.Fatalf("ref updates = %v, want one delete", updates)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("goobers:"+pat))
	for _, auth := range auths {
		if auth != want {
			t.Fatalf("Authorization = %q, want the credentials: PAT as Basic", auth)
		}
	}
}
