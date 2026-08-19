package main

import (
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/instance"
)

// These tests pin the composition-root half of #3274: internal/instance owns
// the workflow-source gitsource seam but cannot import internal/githubapp
// (which imports it), so newWorkflowSourceAppTokenSource is where a
// github-app workflowSource's installation-token minter is constructed —
// mirroring newDaemonIdentityGitHubAppTokenSource. Real minting behavior
// (JWT signing, caching, near-expiry refresh) is internal/githubapp's own
// tested contract; here the constructor's fail-closed edges are what matter.

func workflowSourceAppAuthFixture() instance.WorkflowSource {
	return instance.WorkflowSource{
		Kind: instance.WorkflowSourceKindGit,
		URL:  "https://github.com/example-org/example-config",
		Ref:  "main",
		Auth: &instance.RepoAuthConfig{
			Kind:           instance.GitHubAuthApp,
			AppID:          "123456",
			InstallationID: "10000001",
			PrivateKey:     &instance.TokenRef{File: "/run/secrets/app-key.pem"},
		},
	}
}

func TestNewWorkflowSourceAppTokenSourceBuildsMinter(t *testing.T) {
	tokens, err := newWorkflowSourceAppTokenSource(workflowSourceAppAuthFixture(), nil, nil)
	if err != nil {
		t.Fatalf("newWorkflowSourceAppTokenSource: %v", err)
	}
	if tokens == nil {
		t.Fatal("newWorkflowSourceAppTokenSource returned a nil token source")
	}
}

func TestNewWorkflowSourceAppTokenSourceRejectsNonAppSource(t *testing.T) {
	source := workflowSourceAppAuthFixture()
	source.Auth = nil
	source.Token = &instance.TokenRef{Env: "WORKFLOW_SOURCE_TOKEN"}
	if _, err := newWorkflowSourceAppTokenSource(source, nil, nil); err == nil ||
		!strings.Contains(err.Error(), "does not use github-app auth") {
		t.Fatalf("error = %v, want non-app-source rejection", err)
	}
}

// TestNewWorkflowSourceAppTokenSourceFailsClosedOnStoreBackedKeyWithoutStores
// pins the #683 rule at this seam: a store-backed App key with no store
// resolver is a construction error, never a minter that fails on first use.
func TestNewWorkflowSourceAppTokenSourceFailsClosedOnStoreBackedKeyWithoutStores(t *testing.T) {
	source := workflowSourceAppAuthFixture()
	source.Auth.PrivateKey = &instance.TokenRef{Store: "prod-kv/app-key"}
	if _, err := newWorkflowSourceAppTokenSource(source, nil, nil); err == nil ||
		!strings.Contains(err.Error(), "no secret store resolver is configured") {
		t.Fatalf("error = %v, want fail-closed store-resolver error", err)
	}
}

// TestWorkflowSourceRepositoryName pins the down-scoping derivation: every
// minted installation token is limited to the config repository itself, so
// the repository name must come out of the URL exactly and a URL with no
// derivable name must fail closed rather than mint against the installation's
// whole repository set.
func TestWorkflowSourceRepositoryName(t *testing.T) {
	tests := []struct {
		url     string
		want    string
		wantErr bool
	}{
		{url: "https://github.com/example-org/example-config", want: "example-config"},
		{url: "https://github.com/example-org/example-config.git", want: "example-config"},
		{url: "https://ghes.example.com/org/team-config.git", want: "team-config"},
		{url: "https://github.com", wantErr: true},
		{url: "https://github.com/", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			got, err := workflowSourceRepositoryName(tt.url)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("workflowSourceRepositoryName(%q) = %q, want error", tt.url, got)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("workflowSourceRepositoryName(%q) = %q, %v; want %q", tt.url, got, err, tt.want)
			}
		})
	}
}
