package capability

import (
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// FitsProvider reports whether capability's credential may be materialised
// for an invocation whose repository is on provider. A capability in a
// provider's own namespace (github:*, ado:*) belongs to that provider's
// repositories: an ado:* credential fits only an Azure DevOps repository, and
// a github:* credential fits every repository except an Azure DevOps one. A
// GitHub repository (or a legacy repository reference with no provider) is
// its own namespace; Gitea resolves github:* capabilities against its own
// repository token by design (the rebinding rule of
// docs/design/ado-parity-dsl-2-0.md §3.1), so the credential stays with the
// Gitea repository it was granted for. Provider-neutral capabilities
// (repo:push, provider:*) resolve against the invocation's own repository,
// and non-repository capabilities (agent:model, telemetry:read, ...) are
// independent of it, so both always fit.
//
// The harness applies it when it materialises credentials, and
// `goobers validate` applies it to MCP credential references ahead of a run
// (MCP002), so both answer from this one rule.
func FitsProvider(capability string, provider apiv1.Provider) bool {
	owner, ok := OwnerProvider(capability)
	if !ok {
		return true
	}
	if owner == apiv1.ProviderGitHub {
		return ProviderIsGitHub(provider) || provider == apiv1.ProviderGitea
	}
	return provider == owner
}

// OwnerProvider returns the repository provider whose namespace capability
// belongs to, and false for a provider-neutral or non-repository capability.
func OwnerProvider(capability string) (apiv1.Provider, bool) {
	namespace, _, found := strings.Cut(capability, ":")
	if !found {
		return "", false
	}
	switch provider := apiv1.Provider(namespace); provider {
	case apiv1.ProviderGitHub, apiv1.ProviderADO:
		return provider, true
	}
	return "", false
}

// ProviderIsGitHub treats the legacy empty provider as GitHub, as a
// repository reference did before other providers existed.
func ProviderIsGitHub(provider apiv1.Provider) bool {
	return provider == "" || provider == apiv1.ProviderGitHub
}
