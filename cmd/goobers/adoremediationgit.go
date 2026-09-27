package main

import (
	"context"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/adoauth"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/secretstore"
	"github.com/goobers/goobers/providers"
)

type gitAuthEnvironmentResolver func(context.Context, string) ([]string, error)

var remediationADOCredentialSource = adoauth.Source

func tokenGitAuthEnvironment(token string) gitAuthEnvironmentResolver {
	return func(context.Context, string) ([]string, error) {
		return gitAuthEnv(token), nil
	}
}

func adoRemediationGitAuthEnvironment(root string, routed providers.RepositoryRef) (gitAuthEnvironmentResolver, error) {
	if err := requireCurrentStageMergeAuthority(capability.RepoPush); err != nil {
		return nil, err
	}
	cfg, err := instance.LoadConfig(layoutFor(root).ConfigFile())
	if err != nil {
		return nil, fmt.Errorf("load instance for ADO repository authentication: %w", err)
	}
	configured, ok := configuredRepoForProject(cfg, apiv1.RepoRef{
		Provider: apiv1.ProviderADO,
		Owner:    routed.Owner,
		Project:  routed.Project,
		Name:     routed.Name,
	})
	if !ok || configured.Provider != string(providers.ProviderADO) {
		return nil, fmt.Errorf("ADO remediation repository %s/%s/%s is not configured", routed.Owner, routed.Project, routed.Name)
	}
	authKind := instance.ADOAuthPAT
	if configured.Auth != nil {
		authKind = configured.Auth.Kind
	}
	if authKind == instance.ADOAuthPAT {
		token, err := providerToken(capability.RepoPush)
		if err != nil {
			return nil, err
		}
		source := providers.NewADOPATCredentialSource("goobers", token)
		return func(ctx context.Context, remoteURL string) ([]string, error) {
			return providers.ADOGitAuthEnvironment(ctx, source, nil, remoteURL)
		}, nil
	}
	stores, err := secretstore.NewRegistry(cfg.SecretStores)
	if err != nil {
		return nil, fmt.Errorf("configure ADO repository secret stores: %w", err)
	}
	source, err := remediationADOCredentialSource(configured, nil, stores)
	if err != nil {
		return nil, fmt.Errorf("configure ADO repository authentication: %w", err)
	}
	return func(ctx context.Context, remoteURL string) ([]string, error) {
		return providers.ADOGitAuthEnvironment(ctx, source, nil, remoteURL)
	}, nil
}
