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

// OpenPRRefresher polls the provider on its own interval for open PR heads and
// caches them, so the scheduler's admit-time MaxOpenPRs cap (#353) reads a
// workflow-specific in-memory count instead of making a
// network call under the tick loop's lock (which Admit must never do — it holds
// Conditions.mu and "never fails a tick"). It implements OpenPRCounter.
type OpenPRRefresher struct {
	lister   OpenPRLister
	repo     providers.RepositoryRef
	interval time.Duration

	mu    sync.RWMutex
	heads []string
	known bool
}

// NewOpenPRRefresher builds a refresher over lister for repo. interval <= 0 uses
// DefaultOpenPRRefreshInterval. The count starts "unknown" until the first poll
// completes (Admit fails open until then).
func NewOpenPRRefresher(lister OpenPRLister, repo providers.RepositoryRef, interval time.Duration) *OpenPRRefresher {
	if interval <= 0 {
		interval = DefaultOpenPRRefreshInterval
	}
	return &OpenPRRefresher{lister: lister, repo: repo, interval: interval}
}

// OpenPRCount returns the last polled count of open run-branch PRs for workflow
// and whether that count is known yet. Implements OpenPRCounter.
func (r *OpenPRRefresher) OpenPRCount(workflow string) (int, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.known {
		return 0, false
	}

	prefix := providers.BranchName(workflow, "")
	count := 0
	for _, head := range r.heads {
		if strings.HasPrefix(head, prefix) {
			count++
		}
	}
	return count, true
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
	r.heads = append(r.heads[:0], heads...)
	r.known = true
}
