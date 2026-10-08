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

// Authorization schemes an Azure DevOps credential is sent with. The daemon
// delivers the scheme beside the credential it mints, as a non-secret value,
// so a stage never infers it from the token's shape.
const (
	// SchemeBasic is a PAT, sent as HTTP Basic authentication.
	SchemeBasic = "basic"
	// SchemeBearer is a Microsoft Entra access token, sent as a bearer token.
	SchemeBearer = "bearer"
)

// AuthScheme returns the authorization scheme repo's configured credential
// uses: SchemeBasic for a PAT (the kind an ADO repo without an auth block
// uses), SchemeBearer for every Microsoft Entra identity kind, and "" for a
// repo that is not Azure DevOps or names an unsupported kind. The scheme
// follows from configuration alone, so it can be stated before any token is
// minted.
func AuthScheme(repo instance.RepoRef) string {
	if repo.Provider != string(providers.ProviderADO) {
		return ""
	}
	spec, ok := resolveKind(repo)
	if !ok {
		return ""
	}
	return spec.scheme
}

type kindSpec struct {
	scheme string
	build  func(repo instance.RepoRef, runner providers.CommandRunner, stores credentials.StoreResolver) (providers.ADOCredentialSource, error)
}

// kindSpecs is the single place an ADO auth kind is defined.
var kindSpecs = map[string]kindSpec{
	instance.ADOAuthPAT: {scheme: SchemeBasic, build: func(repo instance.RepoRef, _ providers.CommandRunner, stores credentials.StoreResolver) (providers.ADOCredentialSource, error) {
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
	}},
	instance.ADOAuthAzureCLI: {scheme: SchemeBearer, build: func(repo instance.RepoRef, runner providers.CommandRunner, _ credentials.StoreResolver) (providers.ADOCredentialSource, error) {
		return providers.NewAzureCLIADOCredentialSource(runner, repo.Auth.Tenant), nil
	}},
	instance.ADOAuthWorkloadIdentity: {scheme: SchemeBearer, build: func(repo instance.RepoRef, _ providers.CommandRunner, _ credentials.StoreResolver) (providers.ADOCredentialSource, error) {
		return providers.NewWorkloadIdentityADOCredentialSource(repo.Auth.ClientID)
	}},
	instance.ADOAuthManagedIdentity: {scheme: SchemeBearer, build: func(repo instance.RepoRef, _ providers.CommandRunner, _ credentials.StoreResolver) (providers.ADOCredentialSource, error) {
		return providers.NewManagedIdentityADOCredentialSource(repo.Auth.ClientID)
	}},
}

func repoKind(repo instance.RepoRef) string {
	if repo.Auth != nil {
		return repo.Auth.Kind
	}
	return instance.ADOAuthPAT
}

func resolveKind(repo instance.RepoRef) (kindSpec, bool) {
	spec, ok := kindSpecs[repoKind(repo)]
	return spec, ok
}

// Source builds the configured Azure DevOps credential source. stores
// resolves a store-backed PAT ref (#683); it may be nil only when the PAT is
// env/file-backed — a store ref without it fails closed at construction.
func Source(repo instance.RepoRef, runner providers.CommandRunner, stores credentials.StoreResolver) (providers.ADOCredentialSource, error) {
	if repo.Provider != string(providers.ProviderADO) {
		return nil, fmt.Errorf("ADO credential source requires provider %q, got %q", providers.ProviderADO, repo.Provider)
	}
	spec, ok := resolveKind(repo)
	if !ok {
		return nil, fmt.Errorf("unsupported ADO auth kind %q", repoKind(repo))
	}
	return spec.build(repo, runner, stores)
}

// Provider constructs an ADO provider from one validated instance repository.
func Provider(repo instance.RepoRef, runner providers.CommandRunner, registrar providers.SecretRegistrar, rateObserver providers.RateLimitObserver, quotaObserver providers.QuotaObserver, stores credentials.StoreResolver) (*providers.ADOProvider, error) {
	source, err := Source(repo, runner, stores)
	if err != nil {
		return nil, err
	}
	options := []func(*providers.ADOProvider){
		providers.WithADOCredentialSource(source),
		providers.WithADOSecretRegistrar(registrar),
		providers.WithADORateLimitObserver(rateObserver),
		providers.WithADOQuotaObserver(quotaObserver),
	}
	if runner != nil {
		options = append(options, func(provider *providers.ADOProvider) {
			provider.Runner = runner
		})
	}
	return providers.NewADOProvider(repo.Owner, repo.Project, "", options...), nil
}
