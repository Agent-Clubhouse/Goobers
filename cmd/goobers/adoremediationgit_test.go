package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

type rotatingRemediationADOCredentialSource struct {
	mu    sync.Mutex
	calls int
}

func (s *rotatingRemediationADOCredentialSource) Credential(context.Context) (providers.ADOCredential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return providers.ADOCredential{
		Kind:      "bearer",
		Secret:    "azure-cli-token-" + time.Duration(s.calls).String(),
		ExpiresAt: time.Now().Add(time.Hour),
	}, nil
}

func (s *rotatingRemediationADOCredentialSource) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func useAzureCLIRemediationAuth(t *testing.T, root string) *rotatingRemediationADOCredentialSource {
	t.Helper()
	cfg, err := instance.LoadConfig(layoutFor(root).ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Repos[0].Auth = &instance.RepoAuthConfig{Kind: instance.ADOAuthAzureCLI}
	cfg.Repos[0].Token = instance.TokenRef{}
	if err := instance.WriteConfig(layoutFor(root).ConfigFile(), cfg); err != nil {
		t.Fatal(err)
	}
	source := &rotatingRemediationADOCredentialSource{}
	original := remediationADOCredentialSource
	remediationADOCredentialSource = func(instance.RepoRef, providers.CommandRunner, credentials.StoreResolver) (providers.ADOCredentialSource, error) {
		return source, nil
	}
	t.Cleanup(func() { remediationADOCredentialSource = original })
	return source
}

func TestADORemediationGitAuthEnvironmentUsesConfiguredBearerSourcePerOperation(t *testing.T) {
	root, repo := providerDispatchFixture(t, providers.ProviderADO)
	source := useAzureCLIRemediationAuth(t, root)
	t.Setenv("GOOBERS_CRED_REPO_PUSH", "")

	resolve, err := adoRemediationGitAuthEnvironment(root, repo)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		env, err := resolve(t.Context(), "https://dev.azure.com/acme/project/_git/web")
		if err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(env, "\n")
		if !strings.Contains(joined, "AUTHORIZATION: Bearer azure-cli-token-") {
			t.Fatalf("git environment did not use the Azure CLI bearer scheme: %q", joined)
		}
		if strings.Contains(joined, "AUTHORIZATION: Basic") {
			t.Fatalf("git environment converted an Azure CLI bearer into basic auth: %q", joined)
		}
	}
	if got := source.callCount(); got != 2 {
		t.Fatalf("credential resolutions = %d, want one per Git operation", got)
	}
}
