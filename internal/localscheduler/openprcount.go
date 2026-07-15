package localscheduler

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/goobers/goobers/providers"
)

// DefaultOpenPRRefreshInterval is how often the refresher re-counts open PRs.
// The MaxOpenPRs cap (#353) is a coarse pacing throttle, not a real-time gate,
// so bounded staleness of one interval is fine — and a long-ish interval keeps
// the GitHub call rate trivial. Deliberately off the scheduler's 1s tick.
const DefaultOpenPRRefreshInterval = 45 * time.Second

// openPRPollTimeout bounds a single refresh so a hung GitHub round-trip can't
// wedge the refresher goroutine; on timeout the count simply stays "unknown"
// (fail-open), same as any other poll error.
const openPRPollTimeout = 20 * time.Second

// OpenPRLister is the narrow provider seam the refresher polls — satisfied by
// providers.GitHubProvider.ListOpenPullRequestHeads. Kept minimal so the
// refresher (and Conditions, via OpenPRCounter) stay unit-testable with a fake,
// never a live GitHub client.
type OpenPRLister interface {
	ListOpenPullRequestHeads(ctx context.Context, repo providers.RepositoryRef) ([]string, error)
}

// OpenPRRefresher polls the provider on its own interval for the count of the
// loop's un-merged run-branch PRs and caches it, so the scheduler's admit-time
// MaxOpenPRs cap (#353) reads a cached in-memory count instead of making a
// network call under the tick loop's lock (which Admit must never do — it holds
// Conditions.mu and "never fails a tick"). It implements OpenPRCounter.
//
// It counts every open PR whose head branch is under the goobers/ run-branch
// namespace (providers.BranchName). That equals the implementation workflow's
// own sibling PRs while implementation is the only PR-opening workflow — if
// another PR-producing workflow is ever added, this should bucket by
// per-workflow prefix instead.
type OpenPRRefresher struct {
	lister   OpenPRLister
	repo     providers.RepositoryRef
	prefix   string
	interval time.Duration

	mu    sync.RWMutex
	count int
	known bool
}

// NewOpenPRRefresher builds a refresher over lister for repo. interval <= 0 uses
// DefaultOpenPRRefreshInterval. The count starts "unknown" until the first poll
// completes (Admit fails open until then).
func NewOpenPRRefresher(lister OpenPRLister, repo providers.RepositoryRef, interval time.Duration) *OpenPRRefresher {
	if interval <= 0 {
		interval = DefaultOpenPRRefreshInterval
	}
	// The run-branch namespace prefix, derived from providers.BranchName's own
	// convention (goobers/<workflow>/<run-id>) so it tracks that seam of truth.
	prefix := strings.SplitN(providers.BranchName("x", "x"), "/", 2)[0] + "/"
	return &OpenPRRefresher{lister: lister, repo: repo, prefix: prefix, interval: interval}
}

// OpenPRCount returns the last polled count of open run-branch PRs and whether
// that count is known yet. Cheap in-memory read (Admit calls it under its own
// lock). Implements OpenPRCounter.
func (r *OpenPRRefresher) OpenPRCount() (int, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.count, r.known
}

// Run polls until ctx is cancelled — an eager first poll, then every interval.
// Errors leave the count "unknown" rather than blocking dispatch. Wire it into
// the daemon's background-loop WaitGroup/cancellation at the composition root.
func (r *OpenPRRefresher) Run(ctx context.Context) {
	r.pollOnce(ctx)
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.pollOnce(ctx)
		}
	}
}

func (r *OpenPRRefresher) pollOnce(ctx context.Context) {
	pollCtx, cancel := context.WithTimeout(ctx, openPRPollTimeout)
	defer cancel()
	heads, err := r.lister.ListOpenPullRequestHeads(pollCtx, r.repo)

	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		// Fail open: a transient GitHub error must not stall dispatch, so leave
		// the count "unknown" until a poll succeeds (Admit then skips the cap).
		r.known = false
		return
	}
	n := 0
	for _, h := range heads {
		if strings.HasPrefix(h, r.prefix) {
			n++
		}
	}
	r.count, r.known = n, true
}
