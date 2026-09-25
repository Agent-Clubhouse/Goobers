package harness

import (
	"context"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
)

// A repository credential is only exposed under GitHub-consumed variables when
// the invocation's repository is on GitHub; the model credential is unaffected.
func TestCredentialEnvKeepsRepositoryCredentialsWithTheirProvider(t *testing.T) {
	cases := []struct {
		provider    apiv1.Provider
		wantGHToken bool
	}{
		{provider: apiv1.ProviderGitHub, wantGHToken: true},
		{provider: "", wantGHToken: true},
		{provider: apiv1.ProviderADO, wantGHToken: false},
		{provider: apiv1.ProviderGitea, wantGHToken: false},
	}
	for _, tc := range cases {
		t.Run(string(tc.provider), func(t *testing.T) {
			t.Setenv("AUDIENCE_REPO_TOKEN", "repo-secret")
			t.Setenv("AUDIENCE_MODEL_TOKEN", "model-secret")
			resolver, err := credentials.NewResolver([]credentials.TokenRef{
				{Name: "repo-ref", Env: "AUDIENCE_REPO_TOKEN"},
				{Name: "model-ref", Env: "AUDIENCE_MODEL_TOKEN"},
			})
			if err != nil {
				t.Fatal(err)
			}
			injector, err := credentials.NewInjector(resolver, []credentials.Grant{
				{Capability: "repo:push", Ref: "repo-ref"},
				{Capability: "github:issues:write", Ref: "repo-ref"},
				{Capability: "agent:model", Ref: "model-ref"},
			}, noopRegistrar{})
			if err != nil {
				t.Fatal(err)
			}
			creds, err := injector.Materialize(context.Background(), []string{"repo:push", "github:issues:write", "agent:model"})
			if err != nil {
				t.Fatal(err)
			}
			adapter := &CopilotAdapter{
				Command: []string{"copilot"},
				EnvCapabilities: map[string]string{
					"repo:push":           "GH_TOKEN",
					"github:issues:write": "GITHUB_TOKEN",
					"agent:model":         "COPILOT_GITHUB_TOKEN",
				},
			}
			env := testEnvelope(t.TempDir(), "repo:push", "github:issues:write", "agent:model")
			env.RepoRef = apiv1.RepoRef{Provider: tc.provider, Owner: "example-org", Project: "Example", Name: "example-repo"}
			got, err := adapter.credentialEnv(context.Background(), nil, RunRequest{
				Envelope:    env,
				Workspace:   t.TempDir(),
				Credentials: creds,
			})
			if err != nil {
				t.Fatalf("credentialEnv: %v", err)
			}
			for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
				if has := containsEnv(got, name+"=repo-secret"); has != tc.wantGHToken {
					t.Fatalf("%s injected = %v, want %v (provider %q): %v", name, has, tc.wantGHToken, tc.provider, redactedNames(got))
				}
			}
			if !containsEnv(got, "COPILOT_GITHUB_TOKEN=model-secret") {
				t.Fatalf("model credential not injected for provider %q: %v", tc.provider, redactedNames(got))
			}
		})
	}
}

func redactedNames(env []string) []string {
	names := make([]string, 0, len(env))
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		names = append(names, name)
	}
	return names
}
