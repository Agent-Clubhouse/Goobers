package main

import (
	"context"
	"flag"
	"io"

	"github.com/goobers/goobers/internal/providerconfig"
	"github.com/goobers/goobers/providers"
)

type stageCommandEnv struct {
	root string
	repo providers.RepositoryRef
}

func parseProviderStageCommand(args []string, command string, stderr io.Writer) (stageCommandEnv, bool, int) {
	fs := newCLIFlagSet(command, flag.ContinueOnError)
	return parseProviderStageFlagCommand(fs, args, command, stderr)
}

func parseProviderStageFlagCommand(fs *flag.FlagSet, args []string, command string, stderr io.Writer) (stageCommandEnv, bool, int) {
	env, ok, exitCode := parseProviderStageFlagCommandRoot(fs, args, command, stderr)
	if !ok {
		return stageCommandEnv{}, false, exitCode
	}
	env, ok = resolveProviderStageEnv(env.root, stderr)
	if !ok {
		return stageCommandEnv{}, false, 1
	}
	return env, true, 0
}

func parseProviderStageCommandRoot(args []string, command string, stderr io.Writer) (stageCommandEnv, bool, int) {
	fs := newCLIFlagSet(command, flag.ContinueOnError)
	return parseProviderStageFlagCommandRoot(fs, args, command, stderr)
}

func parseProviderStageFlagCommandRoot(fs *flag.FlagSet, args []string, command string, stderr io.Writer) (stageCommandEnv, bool, int) {
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, command)
	if err := fs.Parse(args); err != nil {
		return stageCommandEnv{}, false, 2
	}
	root, ok := providerStageRootArg(fs)
	if !ok {
		return stageCommandEnv{}, false, 2
	}
	return stageCommandEnv{root: root}, true, 0
}

func parseProviderStageEnv(fs *flag.FlagSet, stderr io.Writer) (stageCommandEnv, bool) {
	root, ok := providerStageRootArg(fs)
	if !ok {
		return stageCommandEnv{}, false
	}
	return resolveProviderStageEnv(root, stderr)
}

func resolveProviderStageEnv(root string, stderr io.Writer) (stageCommandEnv, bool) {
	env, err := loadProviderStageEnv(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return stageCommandEnv{}, false
	}
	return env, true
}

func loadProviderStageEnv(root string) (stageCommandEnv, error) {
	repo, err := providerRepo(root)
	if err != nil {
		return stageCommandEnv{}, err
	}
	return stageCommandEnv{root: root, repo: repo}, nil
}

func (e stageCommandEnv) repoRef() providers.RepositoryRef {
	return e.repo
}

func (e stageCommandEnv) backlogRepoRef() providers.RepositoryRef {
	return backlogRepoRefForStage(e.root, e.repo)
}

func (e stageCommandEnv) backlogProviderRepoRef() providers.RepositoryRef {
	return providerconfig.BacklogProviderRepo(e.repo, e.backlogRepoRef())
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
