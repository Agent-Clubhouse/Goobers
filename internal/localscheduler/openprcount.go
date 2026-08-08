package localscheduler

import (
	"context"
	"sort"
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
// providers.GitHubProvider.ListOpenPullRequests. Kept minimal so the refresher
// (and Conditions, via OpenPRCounter) stay unit-testable with a fake, never a
// live GitHub client.
type OpenPRLister interface {
	ListOpenPullRequests(ctx context.Context, repo providers.RepositoryRef) ([]providers.OpenPRSummary, error)
}

// OpenPRRefresher polls the provider on its own interval for open PR heads and
// caches them, so the scheduler's admit-time MaxOpenPRs cap (#353) reads a
// workflow-specific in-memory count instead of making a
// network call under the tick loop's lock (which Admit must never do — it holds
// Conditions.mu and "never fails a tick"). It implements OpenPRCounter.
type OpenPRRefresher struct {
	lister        OpenPRLister
	repo          providers.RepositoryRef
	interval      time.Duration
	excludeLabels map[string]bool
	// branchNamespaces maps each gaggle to its configured run-branch namespace
	// (#1115). OpenPRCount resolves a gaggle's namespace here so a gaggle that
	// retuned GaggleSpec.BranchNamespace (#965/#1010/#1109) is counted under its
	// own prefix, not the default "goobers/". A gaggle with no entry falls back
	// to the default namespace.
	branchNamespaces map[string]string

	mu    sync.RWMutex
	prs   []providers.OpenPRSummary
	known bool
}

// NewOpenPRRefresher builds a refresher over lister for repo. interval <= 0 uses
// DefaultOpenPRRefreshInterval. excludeLabels are PR labels whose bearers are
// dropped from the count (#986): a PR parked pending a human — goobers:merge-
// escalated — cannot be drained by the daemon, so counting it against the cap
// only starves new work. needs-remediation and plain-open PRs are deliberately
// NOT excluded (the daemon is draining them; the cap must still apply
// backpressure). branchNamespaces maps each gaggle to its run-branch namespace
// (#1115) so OpenPRCount matches each gaggle's own prefix; a nil/empty map (or a
// gaggle absent from it) counts under the default "goobers/" namespace,
// preserving pre-#1115 behavior. The count starts "unknown" until the first poll
// completes (Admit fails open until then).
func NewOpenPRRefresher(lister OpenPRLister, repo providers.RepositoryRef, interval time.Duration, excludeLabels []string, branchNamespaces map[string]string) *OpenPRRefresher {
	if interval <= 0 {
		interval = DefaultOpenPRRefreshInterval
	}
	excluded := make(map[string]bool, len(excludeLabels))
	for _, l := range excludeLabels {
		excluded[l] = true
	}
	return &OpenPRRefresher{lister: lister, repo: repo, interval: interval, excludeLabels: excluded, branchNamespaces: branchNamespaces}
}

// OpenPRCount returns the last polled count of the gaggle's own open run-branch
// PRs for workflow — excluding any carrying an excluded (human-parked) label —
// and whether that count is known yet. Implements OpenPRCounter.
func (r *OpenPRRefresher) OpenPRCount(gaggle, workflow string) (int, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.known {
		return 0, false
	}

	// Match run-branch heads under the gaggle's OWN namespace (#1115). A gaggle
	// that retunes its BranchNamespace (#965/#1010/#1109) opens PRs under its
	// configured prefix rather than the default "goobers/"; resolving the
	// namespace per gaggle here is what makes the cap's backpressure effective
	// for a non-default gaggle instead of silently counting zero. A gaggle absent
	// from branchNamespaces (a single-gaggle default) falls back to the default
	// namespace via BranchNameIn, so its count is unchanged.
	prefix := providers.BranchNameIn(r.branchNamespaces[gaggle], workflow, "")
	count := 0
	for _, pr := range r.prs {
		if !strings.HasPrefix(pr.Head, prefix) {
			continue
		}
		if r.hasExcludedLabel(pr) {
			continue
		}
		count++
	}
	return count, true
}

// hasExcludedLabel reports whether pr carries any label the refresher was told
// to drop from the count (caller holds r.mu).
func (r *OpenPRRefresher) hasExcludedLabel(pr providers.OpenPRSummary) bool {
	for _, l := range pr.Labels {
		if r.excludeLabels[l] {
			return true
		}
	}
	return false
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

// OpenPRRefresherSet routes each gaggle's MaxOpenPRs count to the refresher
// polling that gaggle's OWN repo (#2692). A single refresher over the first
// configured repo left any gaggle bound to another repo counting heads in a
// repo it never opens PRs in — always zero, so its cap was silently
// unenforced. Gaggles sharing a repo share one refresher (one poller per
// distinct repo); a gaggle with no entry reports "unknown", so Admit fails
// open rather than ever reading another repo's count. Implements
// OpenPRCounter.
type OpenPRRefresherSet struct {
	byGaggle map[string]*OpenPRRefresher
	// refreshers holds each distinct refresher exactly once (byGaggle may map
	// several gaggles to one), in deterministic gaggle order, so Run polls
	// every repo once per interval regardless of how many gaggles share it.
	refreshers []*OpenPRRefresher
}

// NewOpenPRRefresherSet builds a set routing each gaggle in byGaggle to its
// repo's refresher. Nil map entries are dropped (that gaggle stays "unknown").
func NewOpenPRRefresherSet(byGaggle map[string]*OpenPRRefresher) *OpenPRRefresherSet {
	set := &OpenPRRefresherSet{byGaggle: make(map[string]*OpenPRRefresher, len(byGaggle))}
	gaggles := make([]string, 0, len(byGaggle))
	for gaggle := range byGaggle {
		gaggles = append(gaggles, gaggle)
	}
	sort.Strings(gaggles)
	seen := make(map[*OpenPRRefresher]bool, len(byGaggle))
	for _, gaggle := range gaggles {
		refresher := byGaggle[gaggle]
		if refresher == nil {
			continue
		}
		set.byGaggle[gaggle] = refresher
		if !seen[refresher] {
			seen[refresher] = true
			set.refreshers = append(set.refreshers, refresher)
		}
	}
	return set
}

// OpenPRCount implements OpenPRCounter by delegating to the gaggle's own
// repo's refresher. A gaggle with no refresher reports "unknown" — Admit fails
// open — never another repo's count. Nil-receiver-safe because the wiring
// hands a possibly-nil *OpenPRRefresherSet to the OpenPRCounter interface.
func (s *OpenPRRefresherSet) OpenPRCount(gaggle, workflow string) (int, bool) {
	if s == nil {
		return 0, false
	}
	refresher, ok := s.byGaggle[gaggle]
	if !ok {
		return 0, false
	}
	return refresher.OpenPRCount(gaggle, workflow)
}

// Run polls every distinct refresher until ctx is cancelled, each on its own
// interval with OpenPRRefresher.Run's eager-first-poll contract. Wire it where
// a single refresher's Run was wired before (the daemon's open-PR loop).
func (s *OpenPRRefresherSet) Run(ctx context.Context) {
	if s == nil {
		return
	}
	var wg sync.WaitGroup
	for _, refresher := range s.refreshers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			refresher.Run(ctx)
		}()
	}
	wg.Wait()
}

func (r *OpenPRRefresher) pollOnce(ctx context.Context) {
	pollCtx, cancel := context.WithTimeout(ctx, openPRPollTimeout)
	defer cancel()
	prs, err := r.lister.ListOpenPullRequests(pollCtx, r.repo)

	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		// Fail open: a transient GitHub error must not stall dispatch, so leave
		// the count "unknown" until a poll succeeds (Admit then skips the cap).
		r.known = false
		return
	}
	r.prs = append(r.prs[:0], prs...)
	r.known = true
}
