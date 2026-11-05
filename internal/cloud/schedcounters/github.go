package schedcounters

import (
	"context"
	"errors"
	"fmt"

	"github.com/goobers/goobers/internal/apireadcache"
	"github.com/goobers/goobers/providers"
)

// GitHubSource opens GitHub providers the way the local daemon's counters do:
// the token is resolved on every call (so rotation takes effect) and registered
// for scrubbing, every response is observed into the quota ledger, every request
// attempt is reserved through it, and a rate limit fails the poll immediately
// instead of sleeping inside a scheduler tick. Transport and 5xx retries stay
// on and each attempt goes through the gate.
type GitHubSource struct {
	// Resolve returns the token for a credential reference ("owner/name"). It has
	// credentials.Resolver's shape.
	Resolve func(ctx context.Context, ref string) (string, error)
	// Register hands each resolved token to the secret scrubber. May be nil.
	Register func([]byte)
	// CacheDir enables the conditional-GET (ETag) read cache, which turns an
	// unchanged list into a zero-quota 304 the accounting refunds. Empty disables
	// it. The cache is partitioned by CacheScope.
	CacheDir   string
	CacheScope apireadcache.Scope
	// Options are applied last; tests point the provider at a local server.
	Options []func(*providers.GitHubProvider)
}

// Open implements Opener.
func (s GitHubSource) Open(ctx context.Context, repo providers.RepositoryRef, access Access) (Provider, error) {
	if s.Resolve == nil {
		return nil, errors.New("schedcounters: GitHub source has no credential resolver")
	}
	if repo.Provider != providers.ProviderGitHub {
		return nil, fmt.Errorf("schedcounters: GitHub source cannot open provider %q", repo.Provider)
	}
	ref := repo.Owner + "/" + repo.Name
	token, err := s.Resolve(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("resolve token for %s: %w", ref, err)
	}
	if s.Register != nil {
		s.Register([]byte(token))
	}
	var opts []func(*providers.GitHubProvider)
	if s.CacheDir != "" {
		opts = append(opts, apireadcache.ScopedGitHubOption(s.CacheDir, "", s.CacheScope))
	}
	if access.Observer != nil {
		opts = append(opts, providers.WithQuotaObserver(access.Observer))
	}
	if access.Gate != nil {
		opts = append(opts, providers.WithQuotaRequestGate(access.Gate))
	}
	opts = append(opts, providers.WithMaxRateLimitRetries(0))
	opts = append(opts, s.Options...)
	return providers.NewGitHubProvider(token, opts...), nil
}
