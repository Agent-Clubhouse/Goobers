package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/secretstore"
	"github.com/goobers/goobers/internal/selfupdate"
)

func TestSelfUpdateCommandUsesReleaseDefaultAndWritesResult(t *testing.T) {
	root := t.TempDir()
	t.Setenv("SELF_UPDATE_TEST_TOKEN", "configured-token")
	config := "apiVersion: goobers.dev/v1alpha1\nkind: Instance\nrepos:\n" +
		"  - provider: github\n    owner: acme\n    name: goobers\n" +
		"    token:\n      env: SELF_UPDATE_TEST_TOKEN\n"
	if err := os.WriteFile(filepath.Join(root, "instance.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	resultPath := filepath.Join(t.TempDir(), "result.json")
	t.Setenv(executor.InstanceRootEnvVar, root)
	t.Setenv(executor.RepoProviderEnvVar, "github")
	t.Setenv(executor.RepoOwnerEnvVar, "acme")
	t.Setenv(executor.RepoNameEnvVar, "goobers")
	t.Setenv(executor.InputEnvVar("resultFile"), resultPath)
	t.Setenv(executor.CredentialEnvVar("github:issues:write"), "issues-token")
	t.Setenv(executor.CredentialEnvVar("contents:read"), "contents-token")

	original := prepareSelfUpdate
	t.Cleanup(func() { prepareSelfUpdate = original })
	var captured selfupdate.PrepareOptions
	prepareSelfUpdate = func(_ context.Context, opts selfupdate.PrepareOptions) (selfupdate.PrepareResult, error) {
		captured = opts
		return selfupdate.PrepareResult{
			UpdateRequested: true,
			Policy:          selfupdate.PolicyOnRelease,
			Target:          "v2",
			StagedPath:      "/staged/goobers",
		}, nil
	}
	var stdout, stderr bytes.Buffer
	if code := runSelfUpdate(nil, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
	if captured.Policy != selfupdate.PolicyOnRelease ||
		captured.Owner != "acme" ||
		captured.Repository != "goobers" ||
		captured.Token != "contents-token" {
		t.Fatalf("options = %+v", captured)
	}
	raw, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatal(err)
	}
	var result selfupdate.PrepareResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if !result.UpdateRequested || result.Target != "v2" {
		t.Fatalf("result = %+v", result)
	}
}

func TestSelfUpdateEscalationUsesIssuesCredentialOverride(t *testing.T) {
	t.Setenv("SELF_UPDATE_REPO_TOKEN", "repo-token")
	t.Setenv("SELF_UPDATE_ISSUES_TOKEN", "issues-token")
	cfg := &instance.Config{
		Credentials: []instance.CredentialGrant{{
			Capability: "github:issues:write",
			Token:      instance.TokenRef{Env: "SELF_UPDATE_ISSUES_TOKEN"},
		}},
	}
	stores, err := secretstore.NewRegistry(nil)
	if err != nil {
		t.Fatal(err)
	}
	token, err := resolveSelfUpdateEscalationToken(context.Background(), cfg, instance.RepoRef{
		Provider: "github",
		Owner:    "acme",
		Name:     "goobers",
		Token:    instance.TokenRef{Env: "SELF_UPDATE_REPO_TOKEN"},
	}, stores)
	if err != nil {
		t.Fatal(err)
	}
	if token != "issues-token" {
		t.Fatalf("token = %q, want issues-token", token)
	}
}
