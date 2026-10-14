package main

import (
	"os"

	"github.com/goobers/goobers/internal/apireadcache"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/providersnapshot"
	"github.com/goobers/goobers/providers"
)

// Wiring for internal/apireadcache (#1053): resolve the instance scheduler dir
// and the current scheduler-evaluation snapshot, then hand them to the cache.
// Persistence is transactional in internal/apireadstore; the former staged
// writeDisk and writeBody filesystem paths no longer exist.

// newCachedGitHubProvider builds a stage GitHub provider whose GETs go through
// the shared conditional-GET cache under root's instance scheduler dir.
func newCachedGitHubProvider(root, token string, opts ...func(*providers.GitHubProvider)) *providers.GitHubProvider {
	cache := apireadcache.ScopedGitHubOption(layoutFor(root).SchedulerDir(), os.Getenv(providersnapshot.EnvVar), stageReadScope())
	return newStageGitHubProvider(token, append(opts, cache)...)
}

// invalidateCurrentProviderSnapshot drops the cached list responses of the
// scheduler evaluation this process runs under, if any.
func invalidateCurrentProviderSnapshot(root string) error {
	return apireadcache.InvalidateScopedSnapshot(layoutFor(root).SchedulerDir(), os.Getenv(providersnapshot.EnvVar), stageReadScope())
}

func stageReadScope() apireadcache.Scope {
	return apireadcache.Scope{Gaggle: os.Getenv(executor.GaggleEnvVar), Binding: os.Getenv(executor.ProviderReadBindingEnvVar), Generation: os.Getenv(executor.ProviderReadGenerationEnvVar)}
}

// Uncached stage reads remain live, but their confirmed writes still invalidate
// already scoped readers in this gaggle. Apply after provider construction so
// the original transport defaults and quota-gate ownership remain unchanged.
func configureStageGitHubMutationCache(root string, provider *providers.GitHubProvider) {
	provider.Client = apireadcache.MutationClient(layoutFor(root).SchedulerDir(), stageReadScope(), providers.ProviderGitHub, provider.Client)
}
