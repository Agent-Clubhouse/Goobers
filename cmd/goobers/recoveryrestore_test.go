package main

import (
	"bytes"
	"testing"

	"github.com/goobers/goobers/internal/instance"
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
