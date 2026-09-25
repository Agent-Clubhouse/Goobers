package harness

import (
	"context"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/mcpconfig"
)

// resolveMCPCredential resolves one external MCP server credentialRef for an
// adapter, returning the invocation-internal key and its token.
//
// A capability-based reference follows the same provider rule as the
// harness's credential environment (CredentialFitsProvider): a credential
// that belongs to another provider's repositories is never materialised, and
// the stage fails closed naming the reference, because the server declared
// that it needs it. A BYO reference (kind: byo) is the operator's own named
// credential for that server and is not a repository capability, so it is
// always materialised.
func resolveMCPCredential(ctx context.Context, adapter string, req RunRequest, server string, ref apiv1.MCPCredentialRef) (key, token string, err error) {
	key = mcpconfig.CredentialKey(ref)
	if ref.Kind != apiv1.MCPCredentialKindBYO {
		provider := req.Envelope.RepoRef.Provider
		if owner, ok := CapabilityProvider(key); ok && !CredentialFitsProvider(key, provider) {
			return key, "", fmt.Errorf(
				"harness: %s: MCP server %q credential capability %q is not materialised: it belongs to %s repositories and this run's repository provider is %s; reference a kind: byo credential for this server, or remove the reference",
				adapter, server, key, owner, providerLabel(provider))
		}
	}
	if req.Credentials == nil {
		return key, "", fmt.Errorf("harness: %s: MCP server %q requires credentials but none were materialized", adapter, server)
	}
	token, err = req.Credentials.Token(ctx, key)
	if err != nil {
		return key, "", fmt.Errorf("harness: %s: resolve MCP server %q credential %q: %w", adapter, server, key, err)
	}
	return key, token, nil
}

// providerLabel names provider for an operator-facing message, rendering the
// legacy empty provider the way providerIsGitHub treats it.
func providerLabel(provider apiv1.Provider) string {
	if provider == "" {
		return "github (no provider set)"
	}
	return string(provider)
}
