package instance

import (
	"reflect"
	"sort"
	"testing"
)

// TestGuardedCredentialPaths is #4273's coverage of the enumeration itself:
// every file-backed TokenRef this config type carries must be collected,
// and nothing else (a store-backed or env-backed ref carries no path to
// guard).
func TestGuardedCredentialPaths(t *testing.T) {
	cfg := &Config{
		Repos: []RepoRef{
			{
				Provider: "github",
				Owner:    "acme",
				Name:     "pat-repo",
				Token:    TokenRef{File: "/creds/pat-repo-token"},
			},
			{
				Provider: "github",
				Owner:    "acme",
				Name:     "app-repo",
				Auth: &RepoAuthConfig{
					Kind:       GitHubAuthApp,
					PrivateKey: &TokenRef{File: "/creds/app-repo-key.pem"},
				},
			},
			{
				// Env-backed token: no path to guard.
				Provider: "github",
				Owner:    "acme",
				Name:     "env-repo",
				Token:    TokenRef{Env: "ACME_ENV_REPO_TOKEN"},
			},
		},
		WorkflowSource: &WorkflowSource{
			Kind:  "git",
			Token: &TokenRef{File: "/creds/workflow-source-token"},
		},
		DaemonIdentity: &DaemonIdentityConfig{
			Kind:       GitHubAuthApp,
			PrivateKey: &TokenRef{File: "/creds/daemon-identity-key.pem"},
		},
		Credentials: []CredentialGrant{
			{Capability: "agent:model", Token: TokenRef{File: "/creds/agent-model-token"}},
			{Capability: "repo:push", Token: TokenRef{Env: "PUSH_TOKEN"}},
		},
	}

	got := GuardedCredentialPaths(cfg)
	sort.Strings(got)
	want := []string{
		"/creds/agent-model-token",
		"/creds/app-repo-key.pem",
		"/creds/daemon-identity-key.pem",
		"/creds/pat-repo-token",
		"/creds/workflow-source-token",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("GuardedCredentialPaths = %v, want %v", got, want)
	}
}

// TestGuardedCredentialPathsNilConfigIsEmpty guards the zero-value/nil
// caller shape a wiring bug could produce.
func TestGuardedCredentialPathsNilConfigIsEmpty(t *testing.T) {
	if got := GuardedCredentialPaths(nil); len(got) != 0 {
		t.Fatalf("GuardedCredentialPaths(nil) = %v, want empty", got)
	}
	if got := GuardedCredentialPaths(&Config{}); len(got) != 0 {
		t.Fatalf("GuardedCredentialPaths(zero-value) = %v, want empty", got)
	}
}
