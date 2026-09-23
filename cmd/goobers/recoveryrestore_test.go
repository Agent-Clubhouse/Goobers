package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

func TestRecoveryRestoreSelectsProviderCompleteIdentity(t *testing.T) {
	config := &instance.Config{Repos: []instance.RepoRef{
		{Provider: "github", Owner: "team", Name: "repo"},
		{Provider: "ado", Owner: "team", Project: "alpha", Name: "repo"},
		{Provider: "ado", Owner: "team", Project: "beta", Name: "repo"},
		{Provider: "gitea", BaseURL: "https://forge.example", Owner: "team", Name: "repo"},
	}}
	for _, expected := range config.Repos {
		key := (providers.RepositoryRef{Provider: providers.ProviderKind(expected.Provider), URL: expected.BaseURL, Owner: expected.Owner, Project: expected.Project, Name: expected.Name}).CanonicalKey()
		got, err := recoveryConfiguredProject(config, key)
		if err != nil || !sameConfiguredRepo(expected, got) {
			t.Fatalf("selected wrong recovery destination: %+v %v", got, err)
		}
	}
	for _, key := range []string{"github|||other|repo|", "ado||unknown|team|repo|", "gitea|other.example||team|repo|"} {
		if _, err := recoveryConfiguredProject(config, key); err == nil {
			t.Fatal("unconfigured recovery identity accepted")
		}
	}
	config.Repos = append(config.Repos, config.Repos[0])
	if _, err := recoveryConfiguredProject(config, "github|||team|repo|"); err == nil {
		t.Fatal("ambiguous recovery configuration accepted")
	}
}

func TestRecoveryRestoreRequiresExplicitRecordDestinationAndBranch(t *testing.T) {
	for _, args := range [][]string{nil, {"--record=x"}, {"--record=x", "--repository=y"}, {"--repository=y", "--branch=z"}} {
		var stdout, stderr bytes.Buffer
		if code := runRecoveryRestore(args, &stdout, &stderr); code != 2 {
			t.Fatalf("missing restore input returned %d", code)
		}
	}
}

func TestRecoveryRestoreGitEnvironmentUsesStageScopedRepoPushCredential(t *testing.T) {
	t.Setenv("GOOBERS_RUN_ID", "receiving-run")
	t.Setenv(executor.CredentialEnvVar(string(capability.RepoPush)), "stage-repo-push-token")
	t.Setenv("GOOBERS_GITHUB_TOKEN", "host-token-must-not-be-read")
	cfg := &instance.Config{
		Repos: []instance.RepoRef{{
			Provider: string(apiv1.ProviderGitHub),
			Owner:    "Agent-Clubhouse",
			Name:     "Goobers",
			Token:    instance.TokenRef{Env: "GOOBERS_GITHUB_TOKEN"},
		}},
	}
	project := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "Agent-Clubhouse", Name: "Goobers"}
	registry, _ := journal.DefaultScrubber()

	env, err := recoveryRestoreGitEnvironment(context.Background(), instance.Layout{}, cfg, project, "https://github.com/Agent-Clubhouse/Goobers.git", registry)
	if err != nil {
		t.Fatalf("recoveryRestoreGitEnvironment: %v", err)
	}
	joined := strings.Join(recoveryAuthenticationEnvironment(env), "\n")
	if !strings.Contains(joined, "AUTHORIZATION: basic") {
		t.Fatalf("stage-scoped repo:push auth not used: %q", joined)
	}
	if strings.Contains(joined, "host-token-must-not-be-read") {
		t.Fatalf("recovery forwarded the configured host token: %q", joined)
	}
	if scrubbed := string(registry.Scrub([]byte("token=stage-repo-push-token"))); strings.Contains(scrubbed, "stage-repo-push-token") {
		t.Fatalf("stage credential was not registered with the scrubber: %q", scrubbed)
	}
}

func TestRecoveryRestoreGitEnvironmentFailsClosedWithoutStageScopedRepoPushCredential(t *testing.T) {
	t.Setenv("GOOBERS_RUN_ID", "receiving-run")
	t.Setenv(executor.CredentialEnvVar(string(capability.RepoPush)), "")
	t.Setenv("GOOBERS_GITHUB_TOKEN", "")
	cfg := &instance.Config{
		Repos: []instance.RepoRef{{
			Provider: string(apiv1.ProviderGitHub),
			Owner:    "Agent-Clubhouse",
			Name:     "Goobers",
			Token:    instance.TokenRef{Env: "GOOBERS_GITHUB_TOKEN"},
		}},
	}
	project := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "Agent-Clubhouse", Name: "Goobers"}
	registry, _ := journal.DefaultScrubber()

	_, err := recoveryRestoreGitEnvironment(context.Background(), instance.Layout{}, cfg, project, "https://github.com/Agent-Clubhouse/Goobers.git", registry)
	if err == nil {
		t.Fatal("recoveryRestoreGitEnvironment succeeded without stage-scoped repo:push credential")
	}
	if !strings.Contains(err.Error(), executor.CredentialEnvVar(string(capability.RepoPush))) {
		t.Fatalf("error = %q, want credential env var %q", err, executor.CredentialEnvVar(string(capability.RepoPush)))
	}
}

func TestRecoveryRestoreConfiguredFileCredentialRemainsSupported(t *testing.T) {
	t.Setenv("GOOBERS_RUN_ID", "")
	t.Setenv(executor.CredentialEnvVar(string(capability.RepoPush)), "")
	tokenFile := filepath.Join(t.TempDir(), "github-token")
	if err := os.WriteFile(tokenFile, []byte("configured-file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &instance.Config{Repos: []instance.RepoRef{{
		Provider: string(apiv1.ProviderGitHub),
		Owner:    "Agent-Clubhouse",
		Name:     "Goobers",
		Token:    instance.TokenRef{File: tokenFile},
	}}}
	project := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "Agent-Clubhouse", Name: "Goobers"}
	registry, _ := journal.DefaultScrubber()
	env, err := recoveryRestoreGitEnvironment(context.Background(), instance.NewLayout(t.TempDir()), cfg, project, "https://github.com/Agent-Clubhouse/Goobers.git", registry)
	if err != nil {
		t.Fatalf("recoveryRestoreGitEnvironment: %v", err)
	}
	if !strings.Contains(strings.Join(env, "\n"), "configured-file-token") {
		t.Fatalf("configured file credential was not used: %q", env)
	}
	if scrubbed := string(registry.Scrub([]byte("token=configured-file-token"))); strings.Contains(scrubbed, "configured-file-token") {
		t.Fatalf("configured file credential was not registered with the scrubber: %q", scrubbed)
	}
}

type recoveryTestSecretStore struct {
	value string
}

func (s recoveryTestSecretStore) FetchSecret(_ context.Context, ref string) (string, error) {
	if ref != "vault/recovery" {
		return "", errors.New("unexpected secret ref")
	}
	return s.value, nil
}

func TestRecoveryConfiguredGitEnvironmentSupportsStoreAndAppSources(t *testing.T) {
	t.Run("secret store", func(t *testing.T) {
		registry, _ := journal.DefaultScrubber()
		repo := instance.RepoRef{
			Provider: "github",
			Owner:    "Agent-Clubhouse",
			Name:     "Goobers",
			Token:    instance.TokenRef{Store: "vault/recovery"},
		}
		resolver, err := githubWorktreeGitEnvironment(t.TempDir(), repo, registry, recoveryTestSecretStore{value: "store-recovery-token"})
		if err != nil {
			t.Fatalf("githubWorktreeGitEnvironment: %v", err)
		}
		env, err := resolver(context.Background(), "https://github.com/Agent-Clubhouse/Goobers.git")
		if err != nil {
			t.Fatalf("configured store resolver: %v", err)
		}
		if !strings.Contains(strings.Join(env, "\n"), "store-recovery-token") {
			t.Fatalf("configured store credential was not used: %q", env)
		}
		if got := string(registry.Scrub([]byte("store-recovery-token"))); strings.Contains(got, "store-recovery-token") {
			t.Fatalf("configured store credential was not registered with scrubber: %q", got)
		}
	})

	t.Run("github app", func(t *testing.T) {
		previous := newGitHubAppTokenSource
		t.Cleanup(func() { newGitHubAppTokenSource = previous })
		newGitHubAppTokenSource = func(instance.RepoRef, credentials.SecretRegistrar, credentials.StoreResolver) (credentials.ExpiringResolveFunc, error) {
			return func(context.Context) (string, time.Time, error) {
				return "app-recovery-token", time.Time{}, nil
			}, nil
		}
		registry, _ := journal.DefaultScrubber()
		repo := instance.RepoRef{
			Provider: "github",
			Owner:    "Agent-Clubhouse",
			Name:     "Goobers",
			Auth: &instance.RepoAuthConfig{
				Kind:           instance.GitHubAuthApp,
				AppID:          "123",
				InstallationID: "456",
				PrivateKey:     &instance.TokenRef{File: filepath.Join(t.TempDir(), "app.pem")},
			},
		}
		resolver, err := githubWorktreeGitEnvironment(t.TempDir(), repo, registry, nil)
		if err != nil {
			t.Fatalf("githubWorktreeGitEnvironment: %v", err)
		}
		env, err := resolver(context.Background(), "https://github.com/Agent-Clubhouse/Goobers.git")
		if err != nil {
			t.Fatalf("configured app resolver: %v", err)
		}
		if !strings.Contains(strings.Join(env, "\n"), "app-recovery-token") {
			t.Fatalf("configured app credential was not used: %q", env)
		}
		if got := string(registry.Scrub([]byte("app-recovery-token"))); strings.Contains(got, "app-recovery-token") {
			t.Fatalf("configured app credential was not registered with scrubber: %q", got)
		}
	})
}

func TestRecoveryAuthenticationEnvironmentExcludesStageAndHostSecrets(t *testing.T) {
	environment := []string{
		"GOOBERS_CRED_REPO_PUSH=stage-repo-push-token",
		"GOOBERS_GITHUB_TOKEN=host-token",
		"GOOBERS_GIT_TOKEN=transport-token",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.https://example.invalid/.extraheader",
		"GIT_CONFIG_VALUE_0=AUTHORIZATION: basic transport-token",
		"PATH=C:\\bin",
	}

	got := strings.Join(recoveryAuthenticationEnvironment(environment), "\n")
	for _, excluded := range []string{
		"GOOBERS_CRED_REPO_PUSH=stage-repo-push-token",
		"GOOBERS_GITHUB_TOKEN=host-token",
		"PATH=C:\\bin",
	} {
		if strings.Contains(got, excluded) {
			t.Fatalf("recovery Git environment forwarded %q: %q", excluded, got)
		}
	}
	for _, allowed := range []string{
		"GOOBERS_GIT_TOKEN=transport-token",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.https://example.invalid/.extraheader",
		"GIT_CONFIG_VALUE_0=AUTHORIZATION: basic transport-token",
	} {
		if !strings.Contains(got, allowed) {
			t.Fatalf("recovery Git environment omitted %q: %q", allowed, got)
		}
	}
}
