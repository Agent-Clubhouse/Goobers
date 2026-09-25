package main

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strconv"
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

// runnerwiring_adocredentials_test.go pins ADO-N17 (#5656,
// docs/design/ado-parity-dsl-2-0.md §4.1): every Azure DevOps auth kind
// resolves in the daemon and backs the repository's grants, including
// repo:push, and every minted value is registered with the scrubber in each
// form it can travel in.

const (
	adoTestOwner = "example-org/example-project"
	adoTestName  = "example-repo"
	adoTestRef   = adoTestOwner + "/" + adoTestName
)

func adoTestRepo(auth *instance.RepoAuthConfig, token instance.TokenRef) instance.RepoRef {
	return instance.RepoRef{
		Provider: "ado",
		Owner:    "example-org",
		Project:  "example-project",
		Name:     adoTestName,
		Auth:     auth,
		Token:    token,
	}
}

// adoTestAzureRunner answers `az account get-access-token` with one token.
type adoTestAzureRunner struct {
	token   string
	expires time.Time
	calls   int
}

func (r *adoTestAzureRunner) Run(_ context.Context, name string, _ ...string) ([]byte, error) {
	r.calls++
	if name != "az" {
		return nil, errors.New("unexpected command " + name)
	}
	return []byte(`{"accessToken":"` + r.token + `","expires_on":` + strconv.FormatInt(r.expires.Unix(), 10) + `}`), nil
}

// adoTestIdentitySource stands in for a workload or managed identity.
type adoTestIdentitySource struct {
	credential providers.ADOCredential
}

func (s adoTestIdentitySource) Credential(context.Context) (providers.ADOCredential, error) {
	return s.credential, nil
}

func stubADOCredentialSource(t *testing.T, build func(instance.RepoRef, credentials.StoreResolver) (providers.ADOCredentialSource, error)) {
	t.Helper()
	previous := newADOCredentialSource
	newADOCredentialSource = build
	t.Cleanup(func() { newADOCredentialSource = previous })
}

func registeredStrings(r *escTestRegistrar) map[string]bool {
	out := make(map[string]bool, len(r.registered))
	for _, value := range r.registered {
		out[string(value)] = true
	}
	return out
}

func TestBuildCredentialsADOEveryKindBacksRepoGrants(t *testing.T) {
	expires := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	patFile := filepath.Join(t.TempDir(), "ado-pat")
	if err := os.WriteFile(patFile, []byte("file-pat-value-0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	identity := func(token string) func(instance.RepoRef, credentials.StoreResolver) (providers.ADOCredentialSource, error) {
		return func(instance.RepoRef, credentials.StoreResolver) (providers.ADOCredentialSource, error) {
			return adoTestIdentitySource{credential: providers.ADOCredential{Kind: providers.ADOCredentialKindBearer, Secret: token, ExpiresAt: expires}}, nil
		}
	}
	azure := &adoTestAzureRunner{token: "entra-cli-token-0123456789", expires: expires}
	for _, tc := range []struct {
		name   string
		repo   instance.RepoRef
		env    map[string]string
		stores credentials.StoreResolver
		build  func(instance.RepoRef, credentials.StoreResolver) (providers.ADOCredentialSource, error)
		token  string
		bearer bool
	}{
		{
			name: "azure-cli",
			repo: adoTestRepo(&instance.RepoAuthConfig{Kind: instance.ADOAuthAzureCLI}, instance.TokenRef{}),
			build: func(repo instance.RepoRef, stores credentials.StoreResolver) (providers.ADOCredentialSource, error) {
				return adoauth.Source(repo, azure, stores)
			},
			token:  azure.token,
			bearer: true,
		},
		{
			name:   "workload-identity",
			repo:   adoTestRepo(&instance.RepoAuthConfig{Kind: instance.ADOAuthWorkloadIdentity}, instance.TokenRef{}),
			build:  identity("entra-workload-token-0123456789"),
			token:  "entra-workload-token-0123456789",
			bearer: true,
		},
		{
			name:   "managed-identity",
			repo:   adoTestRepo(&instance.RepoAuthConfig{Kind: instance.ADOAuthManagedIdentity}, instance.TokenRef{}),
			build:  identity("entra-managed-token-0123456789"),
			token:  "entra-managed-token-0123456789",
			bearer: true,
		},
		{
			name:  "pat from env",
			repo:  adoTestRepo(nil, instance.TokenRef{Env: "EXAMPLE_ADO_PAT"}),
			env:   map[string]string{"EXAMPLE_ADO_PAT": "env-pat-value-0123456789"},
			token: "env-pat-value-0123456789",
		},
		{
			name:  "pat from file",
			repo:  adoTestRepo(&instance.RepoAuthConfig{Kind: instance.ADOAuthPAT}, instance.TokenRef{File: patFile}),
			token: "file-pat-value-0123456789",
		},
		{
			name:   "pat from a secret store",
			repo:   adoTestRepo(&instance.RepoAuthConfig{Kind: instance.ADOAuthPAT}, instance.TokenRef{Store: "example-kv/ado-pat"}),
			stores: wiringFakeStoreResolver{"example-kv/ado-pat": "store-pat-value-0123456789"},
			token:  "store-pat-value-0123456789",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for key, value := range tc.env {
				t.Setenv(key, value)
			}
			if tc.build != nil {
				stubADOCredentialSource(t, tc.build)
			}
			sourceRegistrar := &escTestRegistrar{}
			cfg := &instance.Config{Repos: []instance.RepoRef{tc.repo}}
			resolver, grants, err := buildCredentials(cfg, tc.stores, adoTestOwner, adoTestName, nil, sourceRegistrar)
			if err != nil {
				t.Fatalf("buildCredentials: %v", err)
			}
			refs := make(map[string]string, len(grants))
			for _, grant := range grants {
				refs[grant.Capability] = grant.Ref
			}
			declared := []string{
				string(capability.RepoPush),
				string(capability.GitHubPRWrite),
				string(capability.ProviderPRWrite),
				string(capability.ADOPRComplete),
			}
			for _, name := range declared {
				if refs[name] != adoTestRef {
					t.Fatalf("grant for %s = %q, want the ADO repo's own source %q (grants %+v)", name, refs[name], adoTestRef, grants)
				}
			}

			injector, err := credentials.NewInjector(resolver, grants, &escTestRegistrar{})
			if err != nil {
				t.Fatal(err)
			}
			set, err := injector.Materialize(context.Background(), declared)
			if err != nil {
				t.Fatalf("Materialize: %v", err)
			}
			for _, name := range declared {
				token, err := set.Token(context.Background(), name)
				if err != nil || token != tc.token {
					t.Fatalf("%s token = %q, %v; want %q", name, token, err, tc.token)
				}
			}
			expiry, stated := set.Expiry(string(capability.RepoPush))
			switch {
			case tc.bearer && (!stated || !expiry.Equal(expires)):
				t.Fatalf("repo:push expiry = %v, %v; want the Entra token's %v", expiry, stated, expires)
			case !tc.bearer && stated:
				// A PAT states no expiry: the Set treats it as unbounded, not
				// as already expired.
				t.Fatalf("PAT repo:push expiry = %v, want none stated", expiry)
			}

			want := []string{tc.token, "Bearer " + tc.token}
			if !tc.bearer {
				basic := base64.StdEncoding.EncodeToString([]byte("goobers:" + tc.token))
				want = []string{tc.token, basic, "Basic " + basic}
			}
			registered := registeredStrings(sourceRegistrar)
			for _, form := range want {
				if !registered[form] {
					t.Fatalf("mint-time registrar is missing form %q; registered %d values", form, len(registered))
				}
			}
		})
	}
}

// TestBuildCredentialsDefersADOWorkloadIdentityConstruction proves a host
// without the workload-identity environment can still build credentials (the
// display paths never mint), and that the missing identity surfaces when a
// grant is first resolved.
func TestBuildCredentialsDefersADOWorkloadIdentityConstruction(t *testing.T) {
	for _, name := range []string{"AZURE_TENANT_ID", "AZURE_CLIENT_ID", "AZURE_FEDERATED_TOKEN_FILE"} {
		t.Setenv(name, "")
	}
	repo := adoTestRepo(&instance.RepoAuthConfig{Kind: instance.ADOAuthWorkloadIdentity}, instance.TokenRef{})
	// The premise: building this source eagerly fails on this host.
	if _, err := adoauth.Source(repo, nil, nil); err == nil {
		t.Fatal("premise moved: workload identity construction succeeded without its environment")
	}

	cfg := &instance.Config{Repos: []instance.RepoRef{repo}}
	for _, displayPath := range []credentials.SecretRegistrar{nil, &escTestRegistrar{}} {
		resolver, grants, err := buildCredentials(cfg, nil, adoTestOwner, adoTestName, nil, displayPath)
		if err != nil {
			t.Fatalf("buildCredentials: %v, want construction deferred to first mint", err)
		}
		if len(grants) == 0 {
			t.Fatal("workload identity repo backs no grants")
		}
		if _, err := resolver.Resolve(context.Background(), adoTestRef); err == nil ||
			!strings.Contains(err.Error(), "workload identity") {
			t.Fatalf("Resolve error = %v, want the missing workload identity named", err)
		}
	}
}

// TestADORepositoryTokenSourceRetriesAFailedConstruction proves a transient
// construction failure does not poison the source for the daemon's lifetime.
func TestADORepositoryTokenSourceRetriesAFailedConstruction(t *testing.T) {
	builds := 0
	stubADOCredentialSource(t, func(instance.RepoRef, credentials.StoreResolver) (providers.ADOCredentialSource, error) {
		builds++
		if builds == 1 {
			return nil, errors.New("identity environment not ready")
		}
		return adoTestIdentitySource{credential: providers.ADOCredential{
			Kind: providers.ADOCredentialKindBearer, Secret: "entra-late-token-0123456789", ExpiresAt: time.Now().Add(time.Hour),
		}}, nil
	})
	mint, err := newADORepositoryTokenSource(adoTestRepo(&instance.RepoAuthConfig{Kind: instance.ADOAuthManagedIdentity}, instance.TokenRef{}), nil, nil)
	if err != nil {
		t.Fatalf("newADORepositoryTokenSource: %v", err)
	}
	if _, _, err := mint(context.Background()); err == nil {
		t.Fatal("first mint succeeded, want the construction failure")
	}
	for i := 0; i < 2; i++ {
		if token, _, err := mint(context.Background()); err != nil || token != "entra-late-token-0123456789" {
			t.Fatalf("mint %d = %q, %v", i, token, err)
		}
	}
	if builds != 2 {
		t.Fatalf("source built %d times, want a retry after the failure and reuse after success", builds)
	}
}

// TestADOBasicHeaderIsRedactedFromJournalWrites is the scrubber half of
// ADO-N17: a PAT's Basic Authorization value does not contain the PAT, so it
// is redacted only because the daemon registered that form at mint time.
func TestADOBasicHeaderIsRedactedFromJournalWrites(t *testing.T) {
	t.Setenv("EXAMPLE_ADO_PAT", "journal-pat-value-0123456789")
	registry := journal.NewRegistryScrubber()
	cfg := &instance.Config{Repos: []instance.RepoRef{adoTestRepo(nil, instance.TokenRef{Env: "EXAMPLE_ADO_PAT"})}}
	resolver, grants, err := buildCredentials(cfg, nil, adoTestOwner, adoTestName, nil, registry)
	if err != nil {
		t.Fatal(err)
	}
	injector, err := credentials.NewInjector(resolver, grants, registry)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := injector.Materialize(context.Background(), []string{string(capability.RepoPush)}); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	// The registry alone, not the pattern backstop: the redaction must come
	// from the registered value.
	log, _, err := journal.OpenInstanceLog(dir, journal.WithScrubber(registry))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	basic := base64.StdEncoding.EncodeToString([]byte("goobers:journal-pat-value-0123456789"))
	if err := log.Append(journal.Event{
		Type:   journal.EventRunnerAnnotation,
		Runner: map[string]any{"kind": "test.leak", "note": "request header Authorization: Basic " + basic},
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), basic) {
		t.Fatal("the Basic Authorization value survived into the journal at rest")
	}
	if !strings.Contains(string(data), journal.Redacted) {
		t.Fatal("the leaked header was dropped rather than redacted")
	}
}

// TestBuildCredentialsKeepsADOIdentityReferenceReposOnTheGaggleSource pins the
// reference-repository read grants: an Entra-identity ADO reference repo gets
// none (its checkout keeps authenticating through the gaggle's ADO source, as
// before), while a PAT-backed one keeps its own read grant.
func TestBuildCredentialsKeepsADOIdentityReferenceReposOnTheGaggleSource(t *testing.T) {
	t.Setenv("EXAMPLE_ADO_PAT", "project-pat-value-0123456789")
	t.Setenv("EXAMPLE_REF_PAT", "reference-pat-value-0123456789")
	identityRef := adoTestRepo(&instance.RepoAuthConfig{Kind: instance.ADOAuthAzureCLI}, instance.TokenRef{})
	identityRef.Name = "example-identity-ref"
	patRef := adoTestRepo(nil, instance.TokenRef{Env: "EXAMPLE_REF_PAT"})
	patRef.Name = "example-pat-ref"
	cfg := &instance.Config{Repos: []instance.RepoRef{
		adoTestRepo(nil, instance.TokenRef{Env: "EXAMPLE_ADO_PAT"}),
		identityRef,
		patRef,
	}}
	additional := []apiv1.RepoRef{
		{Provider: apiv1.ProviderADO, Owner: "example-org", Project: "example-project", Name: identityRef.Name},
		{Provider: apiv1.ProviderADO, Owner: "example-org", Project: "example-project", Name: patRef.Name},
	}
	_, grants, err := buildCredentials(cfg, nil, adoTestOwner, adoTestName, additional, nil)
	if err != nil {
		t.Fatal(err)
	}
	refs := make(map[string]string, len(grants))
	for _, grant := range grants {
		refs[grant.Capability] = grant.Ref
	}
	read := string(capability.ContentsRead)
	if ref, ok := refs[credentials.RepoScopedCapability(read, adoTestOwner, identityRef.Name)]; ok {
		t.Fatalf("Entra-identity reference repo gained a read grant %q", ref)
	}
	if ref := refs[credentials.RepoScopedCapability(read, adoTestOwner, patRef.Name)]; ref != adoTestOwner+"/"+patRef.Name {
		t.Fatalf("PAT reference repo read grant = %q, want its own ref", ref)
	}
}

// TestCredentialPlaneResolvesADOAzureCLIRepoPushWithExpiry is the pod half:
// a pod resolve for an azure-cli ADO gaggle returns repo:push with the Entra
// token's expiry, states the bearer scheme, and registers the header form.
func TestCredentialPlaneResolvesADOAzureCLIRepoPushWithExpiry(t *testing.T) {
	expires := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	azure := &adoTestAzureRunner{token: "entra-plane-token-0123456789", expires: expires}
	stubADOCredentialSource(t, func(repo instance.RepoRef, stores credentials.StoreResolver) (providers.ADOCredentialSource, error) {
		return adoauth.Source(repo, azure, stores)
	})
	machine := compileCredentialPlaneMachine(t, credentialPlaneSpec())
	service, shared, runID := newCredentialPlaneFixture(t, machine)
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
	if len(response.Credentials) != 1 || response.Credentials[0].Capability != string(capability.RepoPush) {
		t.Fatalf("credentials = %+v, want repo:push", response.Credentials)
	}
	minted := response.Credentials[0]
	if minted.Value != azure.token {
		t.Fatalf("repo:push value was not the minted Entra token")
	}
	if minted.ExpiresAt == nil || !minted.ExpiresAt.Equal(expires) {
		t.Fatalf("repo:push expiresAt = %v, want %v", minted.ExpiresAt, expires)
	}
	if response.RepoAuthScheme != adoauth.SchemeBearer {
		t.Fatalf("repoAuthScheme = %q, want %q", response.RepoAuthScheme, adoauth.SchemeBearer)
	}
	if got := string(shared.Scrub([]byte("Authorization: Bearer " + azure.token))); strings.Contains(got, azure.token) {
		t.Fatalf("minted bearer header was not registered with the shared scrubber: %q", got)
	}
}

// TestCredentialPlaneStatesNoSchemeForGitHub pins the negative: a GitHub
// gaggle's resolve carries no repository auth scheme.
func TestCredentialPlaneStatesNoSchemeForGitHub(t *testing.T) {
	machine := compileCredentialPlaneMachine(t, credentialPlaneSpec())
	service, _, runID := newCredentialPlaneFixture(t, machine)
	response, err := service.Resolve(context.Background(), httpapi.CredentialResolveRequest{RunID: runID, Stage: "push-branch"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if response.RepoAuthScheme != "" {
		t.Fatalf("repoAuthScheme = %q, want none for a GitHub gaggle", response.RepoAuthScheme)
	}
}

// TestStageCredentialEnvStampsTheADOSchemeWithTheCredentials pins the pod
// stage environment: the scheme travels with the credentials it describes,
// and never on its own.
func TestStageCredentialEnvStampsTheADOSchemeWithTheCredentials(t *testing.T) {
	creds := []dispatcher.MintedCredential{{Capability: "repo:push", Value: "entra-pod-token-0123456789"}}
	got := strings.Join(stageCredentialEnv(creds, adoauth.SchemeBearer), "\n")
	want := executor.CredentialEnvVar("repo:push") + "=entra-pod-token-0123456789\n" + executor.RepoAuthSchemeEnvVar + "=bearer"
	if got != want {
		t.Fatalf("stage credential env = %q, want %q", got, want)
	}
	if env := stageCredentialEnv(creds, ""); len(env) != 1 {
		t.Fatalf("stage credential env without a scheme = %q, want only the credential", env)
	}
	if env := stageCredentialEnv(nil, adoauth.SchemeBearer); len(env) != 0 {
		t.Fatalf("stage credential env with no credentials = %q, want nothing", env)
	}
}

// TestADOCIPollResolvesAStoreBackedPAT proves the daemon-side ADO ci-poll
// provider receives the instance's secret stores, so a store-backed PAT no
// longer fails provider construction.
func TestADOCIPollResolvesAStoreBackedPAT(t *testing.T) {
	cfg := &instance.Config{Repos: []instance.RepoRef{
		adoTestRepo(&instance.RepoAuthConfig{Kind: instance.ADOAuthPAT}, instance.TokenRef{Store: "example-kv/ado-pat"}),
	}}
	stores := wiringFakeStoreResolver{"example-kv/ado-pat": "store-pat-value-0123456789"}
	resolver, grants, err := buildCredentials(cfg, stores, adoTestOwner, adoTestName, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	injector, err := credentials.NewInjector(resolver, grants, &escTestRegistrar{})
	if err != nil {
		t.Fatal(err)
	}
	exec, err := buildCIPollExecutor(cfg, injector, ciPollTestRecorder{}, &cfg.Repos[0], nil, nil, nil, stores)
	if err != nil {
		t.Fatalf("buildCIPollExecutor: %v", err)
	}
	// No prNumber: the run stops at the poll configuration, after the provider
	// was built and before any request is sent.
	_, err = exec.Run(context.Background(), apiv1.InvocationEnvelope{
		RepoRef:      apiv1.RepoRef{Provider: apiv1.ProviderADO, Owner: "example-org", Project: "example-project", Name: adoTestName},
		Capabilities: []string{string(capability.ProviderPRWrite)},
		Inputs:       map[string]interface{}{executor.InputKind: executor.KindCIPoll},
	}, apiv1.DeterministicRun{})
	if err == nil || strings.Contains(err.Error(), "build ADO ci-poll provider") || !strings.Contains(err.Error(), executor.InputPRNumber) {
		t.Fatalf("Run error = %v, want the provider built and the missing %s reported", err, executor.InputPRNumber)
	}
}
