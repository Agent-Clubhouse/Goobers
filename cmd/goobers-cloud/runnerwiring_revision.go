package main

import (
	"context"
	"fmt"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/adoauth"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/workspacerevision"
	"github.com/goobers/goobers/providers"
)

func buildRevisionIdentityResolver(cfg *instance.Config, resolver credentials.Resolver, registrar credentials.SecretRegistrar, stores credentials.StoreResolver) workspacerevision.RepositoryLookup {
	return func(ctx context.Context, route apiv1.RepoRef) (providers.RepositoryMetadata, error) {
		repo, err := configuredRevisionRepository(cfg, route)
		if err != nil {
			return providers.RepositoryMetadata{}, err
		}
		ref := providers.RepositoryRef{Provider: providers.ProviderKind(route.Provider), Owner: repo.Owner, Project: repo.Project, Name: repo.Name}
		if route.Provider == apiv1.ProviderADO {
			provider, err := adoauth.Provider(repo, nil, registrar, nil, nil, stores)
			if err != nil {
				return providers.RepositoryMetadata{}, err
			}
			if repo.BaseURL != "" {
				provider.BaseURL = strings.TrimRight(repo.BaseURL, "/")
			}
			return provider.ReadRepository(ctx, ref)
		}
		token, err := revisionRepositoryToken(ctx, repo, resolver, registrar)
		if err != nil {
			return providers.RepositoryMetadata{}, err
		}
		switch route.Provider {
		case apiv1.ProviderGitHub:
			provider := providers.NewGitHubProvider(token)
			if repo.BaseURL != "" && strings.TrimRight(repo.BaseURL, "/") != "https://github.com" {
				provider.BaseURL = strings.TrimRight(repo.BaseURL, "/") + "/api/v3"
			}
			return provider.ReadRepository(ctx, ref)
		case apiv1.ProviderGitea:
			return providers.NewGiteaProvider(repo.BaseURL, token, providers.WithGiteaSecretRegistrar(registrar)).ReadRepository(ctx, ref)
		default:
			return providers.RepositoryMetadata{}, fmt.Errorf("unsupported repository identity provider %q", route.Provider)
		}
	}
}

func revisionRepositoryToken(ctx context.Context, repo instance.RepoRef, resolver credentials.Resolver, registrar credentials.SecretRegistrar) (string, error) {
	if !repo.Token.Configured() && !repo.GitHubAppAuth() {
		return "", nil
	}
	if resolver == nil {
		return "", fmt.Errorf("repository identity credential resolver is not configured")
	}
	token, err := resolver.Resolve(ctx, repo.Owner+"/"+repo.Name)
	if err != nil {
		return "", err
	}
	if registrar != nil {
		registrar.Register([]byte(token))
	}
	return token, nil
}

func configuredRevisionRepository(cfg *instance.Config, route apiv1.RepoRef) (instance.RepoRef, error) {
	var matched *instance.RepoRef
	if cfg != nil {
		for _, repo := range cfg.Repos {
			provider := repo.Provider
			if provider == "" {
				provider = string(apiv1.ProviderGitHub)
			}
			if provider != string(route.Provider) || !strings.EqualFold(repo.Owner, route.Owner) ||
				!strings.EqualFold(repo.Project, route.Project) || !strings.EqualFold(repo.Name, route.Name) ||
				strings.TrimRight(repo.BaseURL, "/") != strings.TrimRight(route.BaseURL, "/") {
				continue
			}
			if matched != nil {
				return instance.RepoRef{}, fmt.Errorf("repository identity has ambiguous instance routes")
			}
			copy := repo
			matched = &copy
		}
	}
	if matched == nil {
		return instance.RepoRef{}, fmt.Errorf("repository identity route is not declared in instance configuration")
	}
	return *matched, nil
}
