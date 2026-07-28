package main

import (
	"testing"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

func TestProviderRepoRoutesADOFromEnv(t *testing.T) {
	t.Setenv(executor.RepoProviderEnvVar, string(providers.ProviderADO))
	t.Setenv(executor.RepoOwnerEnvVar, "example-org")
	t.Setenv(executor.RepoProjectEnvVar, "Example Service")
	t.Setenv(executor.RepoNameEnvVar, "Example.Repo")

	repo, err := providerRepo(t.TempDir())
	if err != nil {
		t.Fatalf("providerRepo returned error: %v", err)
	}
	if repo.Provider != providers.ProviderADO {
		t.Fatalf("provider = %q, want ado", repo.Provider)
	}
	if repo.Owner != "example-org" || repo.Project != "Example Service" || repo.Name != "Example.Repo" {
		t.Fatalf("unexpected routed ado repo: %#v", repo)
	}
}

func TestProviderRepoRejectsADOMissingProject(t *testing.T) {
	t.Setenv(executor.RepoProviderEnvVar, string(providers.ProviderADO))
	t.Setenv(executor.RepoOwnerEnvVar, "example-org")
	t.Setenv(executor.RepoNameEnvVar, "Example.Repo")

	if _, err := providerRepo(t.TempDir()); err == nil {
		t.Fatal("providerRepo accepted ado repo without a project")
	}
}

func TestProviderRepoRoutesGitHubFromEnv(t *testing.T) {
	t.Setenv(executor.RepoProviderEnvVar, string(providers.ProviderGitHub))
	t.Setenv(executor.RepoOwnerEnvVar, "your-org")
	t.Setenv(executor.RepoNameEnvVar, "your-repo")

	repo, err := providerRepo(t.TempDir())
	if err != nil {
		t.Fatalf("providerRepo returned error: %v", err)
	}
	if repo.Provider != providers.ProviderGitHub || repo.Owner != "your-org" || repo.Name != "your-repo" {
		t.Fatalf("unexpected routed github repo: %#v", repo)
	}
}
