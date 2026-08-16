package main

import (
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

func TestNewProviderForStageDispatchesGitHub(t *testing.T) {
	root := initDemo(t)
	t.Setenv(executor.CredentialEnvVar(string(capability.GitHubIssuesRead)), "read-token")

	provider, err := newProviderForStage(root, providers.RepositoryRef{
		Provider: providers.ProviderGitHub,
		Owner:    "your-org",
		Name:     "your-repo",
	}, true)
	if err != nil {
		t.Fatalf("newProviderForStage: %v", err)
	}
	if provider.Kind() != providers.ProviderGitHub {
		t.Fatalf("provider kind = %q, want %q", provider.Kind(), providers.ProviderGitHub)
	}
}

func TestStageProviderRegistryIncludesBuiltInProviders(t *testing.T) {
	for _, kind := range []providers.ProviderKind{
		providers.ProviderGitHub,
		providers.ProviderADO,
		providers.ProviderGitea,
	} {
		if stageProviderFactories[kind] == nil {
			t.Errorf("provider %q is not registered", kind)
		}
	}
}

func TestNewProviderForStageRejectsUnregisteredProvider(t *testing.T) {
	_, err := newProviderForStage(t.TempDir(), providers.RepositoryRef{Provider: "unknown"}, true)
	if err == nil || !strings.Contains(err.Error(), `provider "unknown" is not registered`) {
		t.Fatalf("error = %v, want unregistered-provider error", err)
	}
}

func TestNewProviderForStageUsesRequestedCapability(t *testing.T) {
	const token = "pr-token"
	t.Setenv(executor.CredentialEnvVar(string(capability.ProviderPRWrite)), token)

	previous := newGitHubProvider
	t.Cleanup(func() { newGitHubProvider = previous })
	var gotToken string
	newGitHubProvider = func(token string, _ ...func(*providers.GitHubProvider)) *providers.GitHubProvider {
		gotToken = token
		return providers.NewGitHubProvider(token)
	}

	_, err := newProviderForStage(
		t.TempDir(),
		providers.RepositoryRef{Provider: providers.ProviderGitHub},
		false,
		withStageProviderCapability(capability.ProviderPRWrite),
	)
	if err != nil {
		t.Fatalf("newProviderForStage: %v", err)
	}
	if gotToken != token {
		t.Fatalf("token = %q, want %q", gotToken, token)
	}
}
