package harness

import (
	"context"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
)

func TestCredentialEnvironmentIsolatedHomeKeepsSplitProviderAuthority(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GH_TOKEN", "ambient-automation")
	t.Setenv("ANTHROPIC_API_KEY", "ambient-model")
	t.Setenv("HUMAN_BACKLOG", "human-backlog")
	resolver, err := credentials.NewResolver([]credentials.TokenRef{{Name: "issues", Env: "HUMAN_BACKLOG"}})
	if err != nil {
		t.Fatal(err)
	}
	injector, err := credentials.NewInjector(resolver, []credentials.Grant{{Capability: "github:issues:read", Ref: "issues"}}, noopRegistrar{})
	if err != nil {
		t.Fatal(err)
	}
	set, err := injector.Materialize(t.Context(), []string{"github:issues:read"})
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	req := RunRequest{IsolatedHome: home, Credentials: set, CredentialAudiences: map[string]apiv1.Provider{"github:issues:read": apiv1.ProviderGitHub}, Envelope: apiv1.InvocationEnvelope{RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderADO}, Capabilities: []string{"github:issues:read"}}}
	env, err := buildCredentialEnv(context.Background(), credentialEnvConfig{extraEnvAllowlist: []string{"GH_TOKEN", "ANTHROPIC_API_KEY"}, envCapabilities: map[string]string{"github:issues:read": "GOOBERS_CRED_GITHUB_ISSUES_READ"}}, req)
	if err != nil {
		t.Fatal(err)
	}
	entries := strings.Join(env, "\n")
	if strings.Contains(entries, "ambient-") || !strings.Contains(entries, "HOME="+home) || !strings.Contains(entries, "GOOBERS_CRED_GITHUB_ISSUES_READ=human-backlog") {
		t.Fatal("isolated environment lost selected identity or inherited ambient credentials")
	}
}
