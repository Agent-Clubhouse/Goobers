package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/secretstore"
	"github.com/goobers/goobers/internal/sharedclaim"
	"github.com/goobers/goobers/providers"
)

// Local lifecycle callers may run without a live daemon. Resolve credentials
// lazily, only when a persisted shared claim actually needs provider cleanup;
// ordinary local cleanup must not start depending on credential availability.
func localLifecycleSharedClaimResolver(layout instance.Layout) claimsclient.SharedClaimResolver {
	return pinnedSharedClaimResolver{layout: layout, store: func(ctx context.Context, repo providers.RepositoryRef) (sharedclaim.Store, error) {
		cfg, err := instance.LoadConfig(layout.ConfigFile())
		if err != nil {
			return nil, err
		}
		stores, err := secretstore.NewRegistry(cfg.SecretStores)
		if err != nil {
			return nil, err
		}
		registry, _ := journal.DefaultScrubber()
		return daemonSharedClaimStore(ctx, cfg, repo, registry, stores)
	}, visibility: func(ctx context.Context, repo providers.RepositoryRef) (sharedclaim.Visibility, error) {
		cfg, err := instance.LoadConfig(layout.ConfigFile())
		if err != nil {
			return nil, err
		}
		stores, err := secretstore.NewRegistry(cfg.SecretStores)
		if err != nil {
			return nil, err
		}
		registry, _ := journal.DefaultScrubber()
		return daemonSharedClaimVisibility(ctx, cfg, repo, registry, stores)
	}}
}

// Daemon admission uses daemon-owned configuration and registered credentials,
// never the environment of a stage or a token supplied in a claim request.
func daemonSharedClaimResolver(layout instance.Layout, cfg *instance.Config, registrar terminalSecretRegistry, stores credentials.StoreResolver) claimsclient.SharedClaimResolver {
	return stageClaimResolver{pinnedSharedClaimResolver{layout: layout, store: func(ctx context.Context, repo providers.RepositoryRef) (sharedclaim.Store, error) {
		return daemonSharedClaimStore(ctx, cfg, repo, registrar, stores)
	}, visibility: func(ctx context.Context, repo providers.RepositoryRef) (sharedclaim.Visibility, error) {
		return daemonSharedClaimVisibility(ctx, cfg, repo, registrar, stores)
	}}}
}

func daemonSharedClaimStore(ctx context.Context, cfg *instance.Config, repo providers.RepositoryRef, registrar terminalSecretRegistry, stores credentials.StoreResolver) (sharedclaim.Store, error) {
	provider, err := daemonSharedClaimProvider(ctx, cfg, repo, registrar, stores, capability.RepoPush)
	if err != nil {
		return nil, err
	}
	return scrubbedSharedClaimStore{store: providers.GitHubSharedClaimStore{Provider: provider, Repository: repo}, registrar: registrar}, nil
}

func daemonSharedClaimVisibility(ctx context.Context, cfg *instance.Config, repo providers.RepositoryRef, registrar terminalSecretRegistry, stores credentials.StoreResolver) (sharedclaim.Visibility, error) {
	provider, err := daemonSharedClaimProvider(ctx, cfg, repo, registrar, stores, capability.GitHubIssuesWrite)
	if err != nil {
		return nil, err
	}
	return scrubbedSharedClaimVisibility{visibility: providers.GitHubSharedClaimVisibility{Provider: provider, Repository: repo}, registrar: registrar}, nil
}

func daemonSharedClaimProvider(ctx context.Context, cfg *instance.Config, repo providers.RepositoryRef, registrar terminalSecretRegistry, stores credentials.StoreResolver, requested capability.Capability) (*providers.GitHubProvider, error) {
	if cfg == nil || repo.Provider != providers.ProviderGitHub {
		return nil, fmt.Errorf("shared claim requires configured GitHub credentials")
	}
	// Credential bindings use owner/name. First narrow by the complete pinned
	// repository identity so an identically named repository on another host
	// cannot provide credentials for this lease.
	scoped := *cfg
	scoped.Repos = nil
	for _, configured := range cfg.Repos {
		candidate := providers.RepositoryRef{Provider: providers.ProviderKind(configured.Provider), URL: configured.BaseURL, Owner: configured.Owner, Project: configured.Project, Name: configured.Name}
		if candidate.CanonicalKey() == repo.CanonicalKey() {
			scoped.Repos = append(scoped.Repos, configured)
		}
	}
	if len(scoped.Repos) != 1 {
		return nil, fmt.Errorf("shared claim requires exactly one configured pinned repository")
	}
	resolver, grants, err := buildCredentials(&scoped, stores, repo.Owner, repo.Name, nil, registrar)
	if err != nil {
		return nil, scrubTerminalError(registrar, err)
	}
	injector, err := credentials.NewInjector(resolver, grants, registrar)
	if err != nil {
		return nil, scrubTerminalError(registrar, err)
	}
	set, err := injector.Materialize(ctx, []string{string(requested)})
	if err != nil {
		return nil, scrubTerminalError(registrar, err)
	}
	provider := providers.NewGitHubProvider("", providers.WithTokenSource(set.For(string(requested))))
	if repo.URL != "" {
		provider.BaseURL = repo.URL
	}
	return provider, nil
}

type scrubbedSharedClaimVisibility struct {
	visibility sharedclaim.Visibility
	registrar  terminalSecretRegistry
}

func (s scrubbedSharedClaimVisibility) ReadClaimed(ctx context.Context, key string) (bool, error) {
	present, err := s.visibility.ReadClaimed(ctx, key)
	return present, scrubTerminalError(s.registrar, err)
}

func (s scrubbedSharedClaimVisibility) SetClaimed(ctx context.Context, key string, present bool) error {
	return scrubTerminalError(s.registrar, s.visibility.SetClaimed(ctx, key, present))
}

type scrubbedSharedClaimStore struct {
	store     sharedclaim.Store
	registrar terminalSecretRegistry
}

func (s scrubbedSharedClaimStore) Read(ctx context.Context, key string) (sharedclaim.Observation, error) {
	observation, err := s.store.Read(ctx, key)
	return observation, s.scrubError(err)
}

func (s scrubbedSharedClaimStore) CompareAndSwap(ctx context.Context, key, revision string, record sharedclaim.Record) error {
	return s.scrubError(s.store.CompareAndSwap(ctx, key, revision, record))
}

func (s scrubbedSharedClaimStore) scrubError(err error) error {
	if err == nil {
		return nil
	}
	scrubbed := scrubTerminalError(s.registrar, err)
	// Keep the coordination and cancellation classifications, but never
	// retain an unsanitized provider error in the public unwrap chain.
	for _, sentinel := range []error{sharedclaim.ErrConflict, sharedclaim.ErrHeld, sharedclaim.ErrNotOwner, context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, sentinel) {
			scrubbed = errors.Join(scrubbed, sentinel)
		}
	}
	return scrubbed
}
