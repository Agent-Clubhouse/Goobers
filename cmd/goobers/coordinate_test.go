package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

func TestCoordinateCheckIsDormantAndNeedsNoCredentials(t *testing.T) {
	root := t.TempDir()
	config := `apiVersion: goobers.dev/v1alpha1
kind: Instance
repos:
  - {provider: github, owner: acme, name: planning, token: {env: UNUSED_COORDINATION_TOKEN}}
  - {provider: github, owner: acme, name: core, token: {env: UNUSED_COORDINATION_TOKEN}}
  - {provider: github, owner: acme, name: consumer, token: {env: UNUSED_COORDINATION_TOKEN}}
coordination:
  gaggles:
    - name: coordinator
      parentRepo: {provider: github, owner: acme, name: planning}
      targets:
        - repository: {provider: github, owner: acme, name: core}
          approval: reviewed-plan
        - repository: {provider: github, owner: acme, name: consumer}
          approval: reviewed-plan
`
	if err := os.WriteFile(filepath.Join(root, "instance.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := filepath.Join("..", "..", "examples", "coordination", "plan.json")
	var stdout, stderr bytes.Buffer
	if code := runCoordinate([]string{"--gaggle", "coordinator", "--plan", plan, "--check", root}, &stdout, &stderr); code != 0 {
		t.Fatalf("code %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"planDigest"`) {
		t.Fatal("missing approval digest")
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != "instance.yaml" {
		t.Fatalf("check created runtime state: %v %v", entries, err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := runCoordinate([]string{"--gaggle", "coordinator", "--plan", plan, root}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "not approved") {
		t.Fatalf("unapproved live pass: %d %s", code, stderr.String())
	}
}

func TestCoordinateRejectsWorkflowAndPodModes(t *testing.T) {
	for _, key := range []string{"GOOBERS_RUN_ID", "GOOBERS_WORKFLOW", "GOOBERS_PROVIDER_SNAPSHOT", "GOOBERS_CLAIMS_ENDPOINT", "KUBERNETES_SERVICE_HOST"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, "test-context")
			var stdout, stderr bytes.Buffer
			if code := runCoordinate([]string{"--gaggle", "coordinator", "--plan", "unused.json", "--check"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "local operator-only") {
				t.Fatalf("unsupported mode accepted: %d %s", code, stderr.String())
			}
		})
	}
	cmd, ok := commandHelp("coordinate")
	if !ok || cmd.providerStage || cmd.tier == cliTierStage {
		t.Fatal("coordinate exposed as a workflow stage")
	}
}

func TestCoordinateProviderUsesExactScopedCredential(t *testing.T) {
	t.Setenv("COORDINATE_TEST_TOKEN", "exact-repo-token")
	t.Setenv("COORDINATE_OTHER_TOKEN", "wrong-repo-token")
	t.Setenv("GH_TOKEN", "ambient-token-must-not-be-used")
	t.Setenv("COORDINATE_OVERRIDE_TOKEN", "broad-token-must-not-be-used")
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer exact-repo-token" {
			t.Error("wrong credential reached provider")
		}
		if r.URL.Path != "/repos/acme/core/issues/7" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"number":7,"title":"child","state":"open"}`))
	}))
	defer server.Close()
	configured := instance.RepoRef{Provider: "github", Owner: "acme", Name: "core", BaseURL: server.URL, Token: instance.TokenRef{Env: "COORDINATE_TEST_TOKEN"}}
	cfg := &instance.Config{
		Repos: []instance.RepoRef{
			{Provider: "github", Owner: "acme", Name: "other", Token: instance.TokenRef{Env: "COORDINATE_OTHER_TOKEN"}},
			configured,
		},
		Credentials: []instance.CredentialGrant{{Capability: "github:issues:write", Token: instance.TokenRef{Env: "COORDINATE_OVERRIDE_TOKEN"}}},
	}
	registry, _ := journal.DefaultScrubber()
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "core", URL: server.URL}
	provider, err := coordinateProvider(context.Background(), cfg, repo, nil, registry)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.GetWorkItem(context.Background(), repo, "7"); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("unexpected provider call count")
	}
	if strings.Contains(scrubTerminalError(registry, contextTokenError{}).Error(), "exact-repo-token") {
		t.Fatal("credential not registered for scrubbing")
	}
	foreign := repo
	foreign.URL = "https://other.invalid"
	if _, err := coordinateProvider(context.Background(), cfg, foreign, nil, registry); err == nil {
		t.Fatal("foreign host acquired credential")
	}
	cfg.Repos = append(cfg.Repos, configured)
	if _, err := coordinateProvider(context.Background(), cfg, repo, nil, registry); err == nil {
		t.Fatal("ambiguous repo acquired credential")
	}
}

type contextTokenError struct{}

func (contextTokenError) Error() string { return "failure exact-repo-token" }
