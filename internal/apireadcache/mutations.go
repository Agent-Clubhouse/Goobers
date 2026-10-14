package apireadcache

import (
	"context"
	"net/http"

	"github.com/goobers/goobers/internal/apireadstore"
	"github.com/goobers/goobers/providers"
)

// ObserveProviderMutation rotates a gaggle/provider read generation after a
// confirmed effect. Old responses, including late fills and ADO read plans,
// cannot refill the new generation. No credentials or mutation body are stored.
// Cache failure never changes the external write result; this wrapper switches
// permanently to live reads if it cannot persist the invalidation.
func (c *apiReadCache) ObserveProviderMutation(_ context.Context, provider providers.ProviderKind) {
	if c == nil || c.schedulerDir == "" || c.mutationScope == "" || c.provider != provider {
		return
	}
	if err := c.withDisk(func(store *apireadstore.Store) error { return store.InvalidateSnapshot(c.mutationScope) }); err != nil {
		c.mutationUnavailable.Store(true)
	}
}

func (c *apiReadCache) scopedReadKey(req *http.Request) (string, error) {
	scope := c.partition + "\x00" + string(c.provider)
	if c.mutationScope != "" {
		var generation string
		err := c.withDisk(func(store *apireadstore.Store) error {
			var err error
			generation, err = store.SnapshotGeneration(c.mutationScope)
			return err
		})
		if err != nil {
			return "", err
		}
		scope += "\x00" + generation
	}
	return scopedRequestKey(scope, req), nil
}

// MutationClient preserves live transport reads while reporting confirmed writes
// to the same gaggle/provider generation as scoped cached readers. Existing
// transport observers keep receiving the notification as well.
func MutationClient(directory string, scope Scope, provider providers.ProviderKind, inner providers.HTTPClient) providers.HTTPClient {
	if inner == nil {
		inner = &http.Client{Timeout: apiReadHTTPTimeout}
	}
	observer := ScopedClient(directory, "", scope, provider, inner).(*apiReadCache)
	return &mutationClient{HTTPClient: inner, observer: observer}
}

type mutationClient struct {
	providers.HTTPClient
	observer providers.ProviderMutationObserver
}

func (c *mutationClient) ObserveProviderMutation(ctx context.Context, kind providers.ProviderKind) {
	c.observer.ObserveProviderMutation(ctx, kind)
	if observer, ok := c.HTTPClient.(providers.ProviderMutationObserver); ok {
		observer.ObserveProviderMutation(ctx, kind)
	}
}
