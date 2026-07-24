// Package adoauth wires instance configuration to Azure DevOps credential
// sources without exposing credentials to workflow or harness environments.
package adoauth

import (
	"context"
	"fmt"

	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

// Source builds the configured Azure DevOps credential source. stores
// resolves a store-backed PAT ref (#683); it may be nil only when the PAT is
// env/file-backed — a store ref without it fails closed at construction.
func Source(repo instance.RepoRef, runner providers.CommandRunner, stores credentials.StoreResolver) (providers.ADOCredentialSource, error) {
	if repo.Provider != "ado" {
		return nil, fmt.Errorf("ADO credential source requires provider %q, got %q", "ado", repo.Provider)
	}
	kind := instance.ADOAuthPAT
	if repo.Auth != nil {
		kind = repo.Auth.Kind
	}
	switch kind {
	case instance.ADOAuthPAT:
		const refName = "ado-repository"
		resolver, err := credentials.NewResolverWithStores([]credentials.TokenRef{
			repo.Token.CredentialTokenRef(refName),
		}, stores)
		if err != nil {
			return nil, fmt.Errorf("configure ADO PAT source: %w", err)
		}
		return providers.NewResolvingADOPATCredentialSource("goobers", func(ctx context.Context) (string, error) {
			return resolver.Resolve(ctx, refName)
		}), nil
	case instance.ADOAuthAzureCLI:
		return providers.NewAzureCLIADOCredentialSource(runner, repo.Auth.Tenant), nil
	case instance.ADOAuthWorkloadIdentity:
		return providers.NewWorkloadIdentityADOCredentialSource()
	case instance.ADOAuthManagedIdentity:
		return providers.NewManagedIdentityADOCredentialSource(repo.Auth.ClientID)
	default:
		return nil, fmt.Errorf("unsupported ADO auth kind %q", kind)
	}
}

// Provider constructs an ADO provider from one validated instance repository.
func Provider(repo instance.RepoRef, runner providers.CommandRunner, registrar providers.SecretRegistrar, observer providers.RateLimitObserver, stores credentials.StoreResolver) (*providers.ADOProvider, error) {
	source, err := Source(repo, runner, stores)
	if err != nil {
		return nil, err
	}
	options := []func(*providers.ADOProvider){
		providers.WithADOCredentialSource(source),
		providers.WithADOSecretRegistrar(registrar),
		providers.WithADORateLimitObserver(observer),
	}
	if runner != nil {
		options = append(options, func(provider *providers.ADOProvider) {
			provider.Runner = runner
		})
	}
	return providers.NewADOProvider(repo.Owner, repo.Project, "", options...), nil
}
