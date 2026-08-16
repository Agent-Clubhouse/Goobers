package main

import (
	"fmt"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/providers"
)

type stageProviderConfig struct {
	root         string
	repo         providers.RepositoryRef
	readOnly     bool
	capability   capability.Capability
	token        string
	cached       bool
	mutationKind string
	openPR       bool
	noRetries    bool
	observeToken func(string)
}

type stageProviderOption func(*stageProviderConfig)

func withStageProviderCapability(cap capability.Capability) stageProviderOption {
	return func(cfg *stageProviderConfig) {
		cfg.capability = cap
	}
}

func withStageProviderToken(token string) stageProviderOption {
	return func(cfg *stageProviderConfig) {
		cfg.token = token
	}
}

func withStageProviderCache() stageProviderOption {
	return func(cfg *stageProviderConfig) {
		cfg.cached = true
	}
}

func withStageProviderMutations(kind string) stageProviderOption {
	return func(cfg *stageProviderConfig) {
		cfg.mutationKind = kind
	}
}

func withStageProviderOpenPR() stageProviderOption {
	return func(cfg *stageProviderConfig) {
		cfg.openPR = true
	}
}

func withStageProviderRetriesDisabled() stageProviderOption {
	return func(cfg *stageProviderConfig) {
		cfg.noRetries = true
	}
}

func withStageProviderTokenObserver(observer func(string)) stageProviderOption {
	return func(cfg *stageProviderConfig) {
		cfg.observeToken = observer
	}
}

type stageProviderFactory func(stageProviderConfig) (providers.Provider, error)

var stageProviderFactories = map[providers.ProviderKind]stageProviderFactory{
	providers.ProviderGitHub: newGitHubProviderForStage,
	providers.ProviderADO:    newRegisteredADOProviderForStage,
	providers.ProviderGitea:  newRegisteredGiteaProviderForStage,
}

func newProviderForStage(root string, repo providers.RepositoryRef, readOnly bool, opts ...stageProviderOption) (providers.Provider, error) {
	cfg := stageProviderConfig{
		root:       root,
		repo:       repo,
		readOnly:   readOnly,
		capability: capability.GitHubIssuesWrite,
	}
	if readOnly {
		cfg.capability = capability.GitHubIssuesRead
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	factory, ok := stageProviderFactories[repo.Provider]
	if !ok {
		return nil, fmt.Errorf("repository provider %q is not registered for stages", repo.Provider)
	}
	return factory(cfg)
}

func newProviderForStageAs[T providers.Provider](root string, repo providers.RepositoryRef, readOnly bool, opts ...stageProviderOption) (T, error) {
	var zero T
	provider, err := newProviderForStage(root, repo, readOnly, opts...)
	if err != nil {
		return zero, err
	}
	typed, ok := provider.(T)
	if !ok {
		return zero, fmt.Errorf("repository provider %q does not support this stage operation", repo.Provider)
	}
	return typed, nil
}

func stageProviderToken(cfg stageProviderConfig) (string, error) {
	var token string
	if cfg.token != "" {
		token = cfg.token
	} else {
		var err error
		token, err = providerToken(cfg.capability)
		if err != nil {
			return "", err
		}
	}
	if cfg.observeToken != nil {
		cfg.observeToken(token)
	}
	return token, nil
}

func newGitHubProviderForStage(cfg stageProviderConfig) (providers.Provider, error) {
	token, err := stageProviderToken(cfg)
	if err != nil {
		return nil, err
	}
	var opts []func(*providers.GitHubProvider)
	if !cfg.readOnly && cfg.mutationKind != "" {
		opts = append(opts, providers.WithMutationRecorder(sidecarMutationRecorder{kind: cfg.mutationKind}))
	}
	if cfg.noRetries {
		opts = append(opts, providers.WithMaxRateLimitRetries(0), providers.WithMaxTransientRetries(0))
	}
	if cfg.cached {
		return newCachedGitHubProvider(cfg.root, token, opts...), nil
	}
	return newGitHubProvider(token, opts...), nil
}

func newRegisteredADOProviderForStage(cfg stageProviderConfig) (providers.Provider, error) {
	if cfg.openPR {
		return newADOProviderForOpenPR(cfg.root, cfg.repo)
	}
	return newADOProviderForStage(cfg.root, cfg.repo)
}

func newRegisteredGiteaProviderForStage(cfg stageProviderConfig) (providers.Provider, error) {
	token, err := stageProviderToken(cfg)
	if err != nil {
		return nil, err
	}
	var opts []func(*providers.GiteaProvider)
	if !cfg.readOnly && cfg.mutationKind != "" {
		opts = append(opts, providers.WithGiteaMutationRecorder(sidecarMutationRecorder{kind: cfg.mutationKind}))
	}
	return newGiteaProviderForStage(cfg.root, cfg.repo, token, opts...)
}
