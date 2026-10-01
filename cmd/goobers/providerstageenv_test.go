package main

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

func TestProviderStageEnvParseFailure(t *testing.T) {
	t.Setenv(executor.RepoProviderEnvVar, "")
	t.Setenv(executor.RepoOwnerEnvVar, "")
	t.Setenv(executor.RepoProjectEnvVar, "")
	t.Setenv(executor.RepoNameEnvVar, "")
	root := t.TempDir()
	configFile := layoutFor(root).ConfigFile()
	if err := os.MkdirAll(filepath.Dir(configFile), 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	if err := os.WriteFile(configFile, []byte("repos: ["), 0o644); err != nil {
		t.Fatalf("write malformed config: %v", err)
	}
	fs := flag.NewFlagSet("provider-stage", flag.ContinueOnError)
	if err := fs.Parse([]string{root}); err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	var stderr bytes.Buffer

	if _, ok := parseProviderStageEnv(fs, &stderr); ok {
		t.Fatal("parseProviderStageEnv succeeded, want failure")
	}
	if !strings.HasPrefix(stderr.String(), "error: ") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestParseProviderStageEnvTooManyArgs(t *testing.T) {
	fs := flag.NewFlagSet("provider-stage", flag.ContinueOnError)
	usageCalled := false
	fs.Usage = func() { usageCalled = true }
	if err := fs.Parse([]string{"one", "two"}); err != nil {
		t.Fatalf("parse: %v", err)
	}

	if _, ok := parseProviderStageEnv(fs, &bytes.Buffer{}); ok {
		t.Fatal("parseProviderStageEnv succeeded, want failure")
	}
	if !usageCalled {
		t.Fatal("usage was not called")
	}
}

func TestParseProviderStageEnvProviderRepoFailure(t *testing.T) {
	t.Setenv(executor.RepoProviderEnvVar, string(providers.ProviderGitHub))
	t.Setenv(executor.RepoOwnerEnvVar, "")
	t.Setenv(executor.RepoProjectEnvVar, "")
	t.Setenv(executor.RepoNameEnvVar, "")
	fs := flag.NewFlagSet("provider-stage", flag.ContinueOnError)
	if err := fs.Parse(nil); err != nil {
		t.Fatalf("parse: %v", err)
	}
	var stderr bytes.Buffer

	if _, ok := parseProviderStageEnv(fs, &stderr); ok {
		t.Fatal("parseProviderStageEnv succeeded, want failure")
	}
	if !strings.Contains(stderr.String(), "error: invalid routed github repository") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestOpenProviderAsProviderOpenFailure(t *testing.T) {
	previous := stageProviderFactories[providers.ProviderGitHub]
	t.Cleanup(func() { stageProviderFactories[providers.ProviderGitHub] = previous })
	stageProviderFactories[providers.ProviderGitHub] = func(stageProviderConfig) (providers.Provider, error) {
		return nil, errors.New("open failed")
	}
	env := stageCommandEnv{repo: providers.RepositoryRef{
		Provider: providers.ProviderGitHub,
		Owner:    "octo",
		Name:     "repo",
	}}

	_, ctx, cancel, err := openProviderAs[providers.Provider](env, true)
	if err == nil || err.Error() != "open failed" {
		t.Fatalf("openProviderAs error = %v, want open failed", err)
	}
	if ctx != nil || cancel != nil {
		t.Fatalf("openProviderAs failure returned context=%v cancel=%v", ctx, cancel)
	}
}

func TestOpenProviderAsTypedProviderMismatch(t *testing.T) {
	previous := stageProviderFactories[providers.ProviderGitHub]
	t.Cleanup(func() { stageProviderFactories[providers.ProviderGitHub] = previous })
	stageProviderFactories[providers.ProviderGitHub] = func(stageProviderConfig) (providers.Provider, error) {
		return providers.NewADOProvider("contoso", "project", "token"), nil
	}
	env := stageCommandEnv{repo: providers.RepositoryRef{
		Provider: providers.ProviderGitHub,
		Owner:    "octo",
		Name:     "repo",
	}}

	_, ctx, cancel, err := openProviderAs[*providers.GitHubProvider](env, true)
	if err == nil || !strings.Contains(err.Error(), `repository provider "github" does not support this stage operation`) {
		t.Fatalf("openProviderAs error = %v", err)
	}
	if ctx != nil || cancel != nil {
		t.Fatalf("openProviderAs mismatch returned context=%v cancel=%v", ctx, cancel)
	}
}
