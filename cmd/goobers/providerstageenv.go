package main

import (
	"context"
	"flag"
	"io"

	"github.com/goobers/goobers/providers"
)

type stageCommandEnv struct {
	root string
	repo providers.RepositoryRef
}

func parseProviderStageEnv(fs *flag.FlagSet, stderr io.Writer) (stageCommandEnv, bool) {
	root, ok := providerStageRootArg(fs)
	if !ok {
		return stageCommandEnv{}, false
	}
	return resolveProviderStageEnv(root, stderr)
}

func resolveProviderStageEnv(root string, stderr io.Writer) (stageCommandEnv, bool) {
	repo, err := providerRepo(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return stageCommandEnv{}, false
	}
	return stageCommandEnv{root: root, repo: repo}, true
}

func (e stageCommandEnv) repoRef() providers.RepositoryRef {
	return e.repo
}

func (e stageCommandEnv) backlogRepoRef() providers.RepositoryRef {
	return backlogRepoRefForStage(e.root, e.repo)
}

func (e stageCommandEnv) backlogProviderRepoRef() providers.RepositoryRef {
	return backlogProviderRepo(e.repo, e.backlogRepoRef())
}

func providerForEnvAs[T any](e stageCommandEnv, readOnly bool, opts ...stageProviderOption) (T, error) {
	return newProviderForStageSurface[T](e.root, e.repo, readOnly, opts...)
}

func openProviderAs[T any](e stageCommandEnv, readOnly bool, opts ...stageProviderOption) (T, context.Context, context.CancelFunc, error) {
	provider, err := providerForEnvAs[T](e, readOnly, opts...)
	if err != nil {
		var zero T
		return zero, nil, nil, err
	}
	ctx, cancel := providerCommandContext()
	return provider, ctx, cancel, nil
}

func openBacklogProviderAs[T any](e stageCommandEnv, readOnly bool, opts ...stageProviderOption) (T, context.Context, context.CancelFunc, error) {
	e.repo = e.backlogProviderRepoRef()
	return openProviderAs[T](e, readOnly, opts...)
}
