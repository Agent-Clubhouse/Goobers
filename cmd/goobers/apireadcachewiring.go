package main

import (
	"os"

	"github.com/goobers/goobers/internal/apireadcache"
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
	cache := apireadcache.Option(layoutFor(root).SchedulerDir(), os.Getenv(providersnapshot.EnvVar))
	return newStageGitHubProvider(token, append(opts, cache)...)
}

// invalidateCurrentProviderSnapshot drops the cached list responses of the
// scheduler evaluation this process runs under, if any.
func invalidateCurrentProviderSnapshot(root string) error {
	return apireadcache.InvalidateSnapshot(layoutFor(root).SchedulerDir(), os.Getenv(providersnapshot.EnvVar))
}
