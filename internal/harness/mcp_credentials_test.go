package harness

import (
	"context"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/mcpconfig"
)

func TestCredentialFitsProviderFollowsTheCapabilityNamespace(t *testing.T) {
	providers := []apiv1.Provider{apiv1.ProviderGitHub, "", apiv1.ProviderADO, apiv1.ProviderGitea}
	cases := []struct {
		capability string
		fits       map[apiv1.Provider]bool
	}{
		{capability: "github:issues:write", fits: map[apiv1.Provider]bool{apiv1.ProviderGitHub: true, "": true}},
		{capability: "github:issues:approve", fits: map[apiv1.Provider]bool{apiv1.ProviderGitHub: true, "": true}},
		{capability: "github:milestones:write", fits: map[apiv1.Provider]bool{apiv1.ProviderGitHub: true, "": true}},
		{capability: "ado:pr:complete", fits: map[apiv1.Provider]bool{apiv1.ProviderADO: true}},
		{capability: "repo:push", fits: map[apiv1.Provider]bool{apiv1.ProviderGitHub: true, "": true, apiv1.ProviderADO: true, apiv1.ProviderGitea: true}},
		{capability: "provider:pr:write", fits: map[apiv1.Provider]bool{apiv1.ProviderGitHub: true, "": true, apiv1.ProviderADO: true, apiv1.ProviderGitea: true}},
		{capability: "contents:read", fits: map[apiv1.Provider]bool{apiv1.ProviderGitHub: true, "": true, apiv1.ProviderADO: true, apiv1.ProviderGitea: true}},
		{capability: "agent:model", fits: map[apiv1.Provider]bool{apiv1.ProviderGitHub: true, "": true, apiv1.ProviderADO: true, apiv1.ProviderGitea: true}},
	}
	for _, tc := range cases {
		for _, provider := range providers {
			if got := CredentialFitsProvider(tc.capability, provider); got != tc.fits[provider] {
				t.Errorf("CredentialFitsProvider(%q, %q) = %v, want %v", tc.capability, provider, got, tc.fits[provider])
			}
		}
	}
}

// mcpPreparer materialises req's external MCP servers through one adapter and
// returns the environment entries that carry resolved credentials.
type mcpPreparer func(t *testing.T, req RunRequest) ([]string, error)

var mcpPreparers = map[string]mcpPreparer{
	"claude-code": func(_ *testing.T, req RunRequest) ([]string, error) {
		_, env, err := prepareClaudeMCP(context.Background(), req)
		return env, err
	},
	"copilot-cli": func(_ *testing.T, req RunRequest) ([]string, error) {
		return prepareCopilotMCP(context.Background(), req, nil)
	},
	"codex": func(t *testing.T, req RunRequest) ([]string, error) {
		env, _, _, err := prepareCodexMCP(context.Background(), req, t.TempDir(), "", nil, nil, true)
		return env, err
	},
}

func envCarriesSecret(env []string, secret string) bool {
	for _, entry := range env {
		if strings.HasSuffix(entry, secret) {
			return true
		}
	}
	return false
}

func remoteMCPServer(name string, ref apiv1.MCPCredentialRef) apiv1.MCPServer {
	ref.Header = "Authorization"
	ref.Scheme = apiv1.MCPHeaderSchemeBearer
	return apiv1.MCPServer{Name: name, URL: "https://" + name + ".example.test/mcp", CredentialRefs: []apiv1.MCPCredentialRef{ref}}
}

// A capability-based MCP credential is materialised only on a repository of
// the provider the capability belongs to; a BYO credential is the operator's
// own and is materialised on every provider.
func TestMCPCredentialRefsFollowTheRepositoryProvider(t *testing.T) {
	cases := []struct {
		provider   apiv1.Provider
		capability string
		wantFits   bool
	}{
		{provider: apiv1.ProviderGitHub, capability: "github:issues:write", wantFits: true},
		{provider: "", capability: "github:issues:write", wantFits: true},
		{provider: apiv1.ProviderADO, capability: "github:issues:write", wantFits: false},
		{provider: apiv1.ProviderGitea, capability: "github:issues:write", wantFits: false},
		{provider: apiv1.ProviderADO, capability: "ado:pr:complete", wantFits: true},
		{provider: apiv1.ProviderGitHub, capability: "ado:pr:complete", wantFits: false},
		{provider: apiv1.ProviderADO, capability: "repo:push", wantFits: true},
		{provider: apiv1.ProviderGitea, capability: "repo:push", wantFits: true},
	}
	for adapter, prepare := range mcpPreparers {
		for _, tc := range cases {
			t.Run(adapter+"/"+string(tc.provider)+"/"+tc.capability, func(t *testing.T) {
				workspace := t.TempDir()
				envelope := testEnvelope(workspace, tc.capability)
				envelope.RepoRef = apiv1.RepoRef{Provider: tc.provider, Owner: "example-org", Project: "example-project", Name: "example-repo"}
				req := RunRequest{
					Envelope:  envelope,
					Workspace: workspace,
					Credentials: mcpTestCredentials(t,
						tc.capability, "repository-mcp-secret",
						mcpconfig.BYOCredentialKey("vendor-api"), "vendor-mcp-secret",
					),
					MCPServers: []apiv1.MCPServer{
						remoteMCPServer("repository-context", apiv1.MCPCredentialRef{Capability: tc.capability}),
						remoteMCPServer("vendor-context", apiv1.MCPCredentialRef{Kind: apiv1.MCPCredentialKindBYO, Ref: "vendor-api"}),
					},
				}
				env, err := prepare(t, req)
				if !tc.wantFits {
					if err == nil {
						t.Fatalf("MCP credential %q materialised for provider %q, want a closed failure", tc.capability, tc.provider)
					}
					for _, want := range []string{`"repository-context"`, `"` + tc.capability + `"`, "kind: byo"} {
						if !strings.Contains(err.Error(), want) {
							t.Fatalf("error %q does not name %s", err, want)
						}
					}
					if strings.Contains(err.Error(), "repository-mcp-secret") {
						t.Fatalf("error carries the credential: %v", err)
					}
					if envCarriesSecret(env, "repository-mcp-secret") {
						t.Fatalf("withheld credential reached the environment: %v", redactedNames(env))
					}
					return
				}
				if err != nil {
					t.Fatalf("prepare: %v", err)
				}
				for _, secret := range []string{"repository-mcp-secret", "vendor-mcp-secret"} {
					if !envCarriesSecret(env, secret) {
						t.Fatalf("credential for %s not materialised: %v", secret, redactedNames(env))
					}
				}
			})
		}
	}
}

// A BYO credential is not a repository capability: a server that references
// only BYO credentials is materialised on a repository of any provider.
func TestMCPBYOCredentialRefsMaterialiseOnEveryProvider(t *testing.T) {
	for adapter, prepare := range mcpPreparers {
		for _, provider := range []apiv1.Provider{apiv1.ProviderGitHub, "", apiv1.ProviderADO, apiv1.ProviderGitea} {
			t.Run(adapter+"/"+string(provider), func(t *testing.T) {
				workspace := t.TempDir()
				envelope := testEnvelope(workspace)
				envelope.RepoRef = apiv1.RepoRef{Provider: provider, Owner: "example-org", Project: "example-project", Name: "example-repo"}
				env, err := prepare(t, RunRequest{
					Envelope:    envelope,
					Workspace:   workspace,
					Credentials: mcpTestCredentials(t, mcpconfig.BYOCredentialKey("vendor-api"), "vendor-mcp-secret"),
					MCPServers: []apiv1.MCPServer{
						remoteMCPServer("vendor-context", apiv1.MCPCredentialRef{Kind: apiv1.MCPCredentialKindBYO, Ref: "vendor-api"}),
					},
				})
				if err != nil {
					t.Fatalf("prepare: %v", err)
				}
				if !envCarriesSecret(env, "vendor-mcp-secret") {
					t.Fatalf("BYO credential not materialised: %v", redactedNames(env))
				}
			})
		}
	}
}
