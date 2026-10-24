// Package schedcounters supplies the provider-backed demand counters and the
// open-PR cap counter the cloud control role's scheduler entries need (L1d).
// It ports the pure, runner-free half of the local daemon's counter wiring: the
// local counters are built in package main around a worktree-and-scheduler-dir
// world (read cache, quota file, claim ledger file) that the cloud does not
// have, so what is shared here is the selection logic, and everything that
// touches a forge goes through one injected seam.
//
// Credentials stay outside the counters: they only see an Opener, which
// resolves (and scrubs) the token on every poll so rotation takes effect.
// GitHubSource is the GitHub opener; it also applies the poll's quota Access.
//
// Quota: when Deps.Quota is set every poll spends and feeds that ledger exactly
// as the local daemon's counters do (observer, request gate, prepaid
// reservation, no in-tick rate-limit sleeping), and the open-PR refresher
// reserves its polls from it. L1e must hand the SAME state to the scheduler and
// to observationsFor(state); see Deps.Quota.
//
// The schedule-demand counter (scheduled update-behind-pr remediation) sizes a
// due fire to the pull requests the remediation stages would actually select. It
// runs the same selection the stages run (internal/remediationselect) over the
// cloud claims namespace and the cloud journal's branch ownership.
package schedcounters

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/cloud/schedentries"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/remediationselect"
	"github.com/goobers/goobers/internal/runtimewiring"
	"github.com/goobers/goobers/providers"
)

// Opener builds a provider for one poll.
type Opener func(ctx context.Context, repo providers.RepositoryRef, access Access) (Provider, error)

// Access is the quota accounting of ONE poll: the provider it opens must report
// every response to Observer and reserve every request attempt through Gate,
// and must not sleep out a rate limit inside the poll (the scheduler waits for
// the next reset-aware admission instead). Both are nil when no quota ledger is
// configured.
type Access struct {
	Observer providers.QuotaObserver
	Gate     providers.QuotaRequestGate
}

// Provider is the forge surface the counters read. The embedded
// remediationselect.Provider is what the shared remediation selection reads.
type Provider interface {
	remediationselect.Provider
	ListWorkItems(ctx context.Context, req providers.ListWorkItemsRequest) ([]providers.WorkItem, error)
	ListOpenPullRequests(ctx context.Context, repo providers.RepositoryRef) ([]providers.OpenPRSummary, error)
	ListPullRequests(ctx context.Context, req providers.ListPullRequestsRequest) ([]providers.PullRequestSummary, error)
}

// Deps binds one applied generation's counters.
type Deps struct {
	// Config and Set are the applied generation's policy and definitions (the same
	// pair schedentries.Input carries).
	Config *instance.Config
	Set    *instance.ConfigSet
	// Open returns a provider for repo with freshly resolved credentials. It is
	// called exactly once per poll, never cached, so a rotated token is picked
	// up. It MUST honour the Access it is handed (see Access); GitHubSource.Open
	// does, and is the way to build one.
	Open Opener
	// Claims and Journal feed the schedule-demand counter: the PR claim namespace
	// of the cloud claims plane and the cross-run journal read behind branch
	// ownership. Both are required for a workflow that needs schedule demand; a
	// workflow that does not is unaffected, and one that does but has neither gets
	// no counter (so schedentries keeps it disabled with its pending reason) and
	// the cause is reported once.
	Claims  remediationselect.NamespaceLister
	Journal remediationselect.BranchOwnershipReader
	// Now is the clock claim expiry is judged against. Defaults to time.Now.
	Now func() time.Time
	// Quota is the provider quota ledger. L1e MUST pass the very state it gives
	// the scheduler (localscheduler.WithProviderQuota) and builds the observations
	// ledger from (observationsFor(state)): the scheduler budgets polls against it
	// and the counters feed it the rate-limit headers they see, so a private copy
	// here would let polling spend quota the scheduler believes it still has. nil
	// disables accounting (tests, or an instance with no quota tracking).
	Quota *localscheduler.ProviderQuotaState
	// SelfIdentity is the login a respectAssignee backlog-query matches.
	SelfIdentity string
	// OpenPRInterval overrides localscheduler.DefaultOpenPRRefreshInterval.
	OpenPRInterval time.Duration
	// OpenPRMaxStale bounds how long a refresher shed by the quota ledger keeps
	// serving its last list. Zero means maxStaleIntervals refresh intervals.
	OpenPRMaxStale time.Duration
	// Report receives refresh failures that leave a count unknown. May be nil.
	Report func(error)
}

// Counters are one generation's counters. It is safe for concurrent use.
type Counters struct {
	deps    Deps
	gaggles map[string]apiv1.Gaggle
	open    openCounter

	mu    sync.Mutex
	built map[localscheduler.WorkflowIdentity]schedentries.Counters
}

// New validates the deps and builds the generation's open-PR refreshers.
func New(deps Deps) (*Counters, error) {
	if deps.Config == nil || deps.Set == nil || deps.Open == nil {
		return nil, errors.New("schedcounters: config, definitions and a provider opener are required")
	}
	if deps.Report == nil {
		deps.Report = func(error) {}
	}
	c := &Counters{deps: deps, gaggles: map[string]apiv1.Gaggle{}, built: map[localscheduler.WorkflowIdentity]schedentries.Counters{}}
	for _, gaggle := range deps.Set.Gaggles {
		c.gaggles[gaggle.Name] = gaggle
	}
	// Validate the capped repositories now; Run builds a fresh set per epoch.
	if _, err := c.buildOpenPRs(); err != nil {
		return nil, err
	}
	return c, nil
}

// For returns the counters of one workflow: Backlog for a backlog-item trigger,
// RefillDemand for a desired-concurrency backlog-query workflow. A counter the
// workflow does not need stays nil, as does the schedule-demand counter until it
// exists, and schedentries disables the entry with that counter's own pending
// reason only. A workflow whose selection cannot be compiled yields no counter
// (the entry stays disabled with its pending reason) and the cause is reported.
func (c *Counters) For(identity localscheduler.WorkflowIdentity) schedentries.Counters {
	c.mu.Lock()
	defer c.mu.Unlock()
	if built, ok := c.built[identity]; ok {
		return built
	}
	built, err := c.build(identity)
	if err != nil {
		c.deps.Report(fmt.Errorf("schedcounters: %s/%s: %w", identity.Gaggle, identity.Workflow, err))
	}
	c.built[identity] = built
	return built
}

// OpenPRs is the counter behind readiness.maxOpenPRs. It reports unknown (so
// admission fails open) until a refresher's first poll completes in the current
// Run, and always for a gaggle with no refresher.
func (c *Counters) OpenPRs() localscheduler.OpenPRCounter { return &c.open }

// Run polls the open-PR refreshers until ctx ends and then returns. The caller
// binds ctx to the leadership epoch. Every Run starts from fresh refreshers, so
// a count measured in an earlier epoch is never served: it reads unknown until
// this Run's first poll lands. The counter keeps the last count it served when
// the quota ledger sheds a poll, but only within one Run.
func (c *Counters) Run(ctx context.Context) error {
	set, err := c.buildOpenPRs()
	if err != nil {
		return err
	}
	c.open.install(set)
	defer c.open.install(nil)
	if set != nil {
		set.Run(ctx)
	}
	<-ctx.Done()
	return nil
}

// openCounter serves the current Run's refresher set.
type openCounter struct {
	mu  sync.RWMutex
	set *localscheduler.OpenPRRefresherSet
}

func (o *openCounter) install(set *localscheduler.OpenPRRefresherSet) {
	o.mu.Lock()
	o.set = set
	o.mu.Unlock()
}

func (o *openCounter) OpenPRCount(gaggle, workflow string) (int, bool) {
	o.mu.RLock()
	set := o.set
	o.mu.RUnlock()
	return set.OpenPRCount(gaggle, workflow)
}

func (c *Counters) workflow(identity localscheduler.WorkflowIdentity) (*apiv1.Workflow, apiv1.Gaggle, bool) {
	gaggle, ok := c.gaggles[identity.Gaggle]
	if !ok {
		return nil, apiv1.Gaggle{}, false
	}
	for i := range c.deps.Set.Workflows {
		wf := &c.deps.Set.Workflows[i]
		if wf.Name == identity.Workflow && wf.Spec.Gaggle == identity.Gaggle {
			return wf, gaggle, true
		}
	}
	return nil, apiv1.Gaggle{}, false
}

func (c *Counters) build(identity localscheduler.WorkflowIdentity) (schedentries.Counters, error) {
	wf, gaggle, ok := c.workflow(identity)
	if !ok || len(c.deps.Config.Repos) == 0 {
		return schedentries.Counters{}, nil
	}
	var out schedentries.Counters
	backlog, err := c.backlog(gaggle, wf)
	if err != nil {
		return out, err
	}
	if backlog != nil {
		out.Backlog = backlog
	}
	refill, err := c.refill(gaggle, wf)
	if err != nil {
		return out, err
	}
	if refill != nil {
		out.RefillDemand = refill
	}
	demand, err := c.scheduleDemand(gaggle, wf)
	if err != nil {
		return out, err
	}
	if demand != nil {
		out.ScheduleDemand = demand
	}
	return out, nil
}

func (c *Counters) buildOpenPRs() (*localscheduler.OpenPRRefresherSet, error) {
	capped := map[string]bool{}
	for i := range c.deps.Set.Workflows {
		if c.deps.Set.Workflows[i].Spec.Readiness.MaxOpenPRs > 0 {
			capped[c.deps.Set.Workflows[i].Spec.Gaggle] = true
		}
	}
	if len(capped) == 0 || len(c.deps.Config.Repos) == 0 {
		return nil, nil
	}
	namespaces := runtimewiring.BranchNamespacesByGaggle(c.deps.Set)
	byRepo := map[string]*localscheduler.OpenPRRefresher{}
	byGaggle := map[string]*localscheduler.OpenPRRefresher{}
	names := make([]string, 0, len(capped))
	for name := range capped {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		repo, err := c.repoFor(c.gaggles[name].Spec.Project)
		if err != nil {
			return nil, fmt.Errorf("gaggle %q readiness.maxOpenPRs: %w", name, err)
		}
		key := string(repo.Provider) + ":" + repo.Owner + "/" + repo.Project + "/" + repo.Name
		refresher := byRepo[key]
		if refresher == nil {
			// goobers:merge-escalated marks a PR parked pending a human; it cannot
			// be drained, so counting it only starves new work (#986).
			refresher = localscheduler.NewOpenPRRefresher(
				newLister(c.deps), repo, c.deps.OpenPRInterval,
				[]string{providers.LabelMergeEscalated}, namespaces)
			byRepo[key] = refresher
		}
		byGaggle[name] = refresher
	}
	return localscheduler.NewOpenPRRefresherSet(byGaggle), nil
}

// repoFor resolves the repository a gaggle's project is bound to exactly as the
// local daemon does: the configured binding wins, an unbound project is polled
// under its own owner/name (its credential resolution then fails per poll and
// the count stays unknown rather than reading another repo's PRs), and a
// project naming no repository falls back to the first configured one.
func (c *Counters) repoFor(project apiv1.RepoRef) (providers.RepositoryRef, error) {
	repo := c.deps.Config.Repos[0]
	if configured, ok := runtimewiring.ConfiguredRepoForProject(c.deps.Config, project); ok {
		repo = configured
	} else if project.Owner != "" && project.Name != "" {
		repo = instance.RepoRef{Owner: project.Owner, Name: project.Name, Provider: string(project.Provider)}
	}
	switch repo.Provider {
	case "", string(providers.ProviderGitHub):
		return providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: repo.Owner, Name: repo.Name}, nil
	case string(providers.ProviderADO):
		return providers.RepositoryRef{Provider: providers.ProviderADO, Owner: repo.Owner, Project: repo.Project, Name: repo.Name}, nil
	default:
		return providers.RepositoryRef{}, fmt.Errorf("provider %q is not supported", repo.Provider)
	}
}

// maxStaleIntervals bounds how long a shed refresher keeps serving its last
// list: a quota window reporting a bogus far-future reset must not freeze the
// cap on an old count indefinitely.
const maxStaleIntervals = 5

// lister adapts the per-call opener to the refresher's lister and routes every
// poll through the quota ledger. A poll the ledger sheds serves the last list
// this lister read, until the window's reset or maxStaleIntervals refresh
// intervals have passed, whichever is sooner: a failed poll leaves the
// refresher's count unknown, and an unknown count lifts the cap, which would
// turn a quota shortage into unthrottled dispatch. Past that bound the shed
// poll fails and the count goes unknown, as it does for any other failed poll.
type lister struct {
	open     Opener
	quota    *localscheduler.ProviderQuotaState
	maxStale time.Duration
	now      func() time.Time

	mu   sync.Mutex
	last []providers.OpenPRSummary
	at   time.Time
	have bool
}

func newLister(deps Deps) *lister {
	interval := deps.OpenPRInterval
	if interval <= 0 {
		interval = localscheduler.DefaultOpenPRRefreshInterval
	}
	maxStale := deps.OpenPRMaxStale
	if maxStale <= 0 {
		maxStale = maxStaleIntervals * interval
	}
	return &lister{open: deps.Open, quota: deps.Quota, maxStale: maxStale, now: time.Now}
}

func (l *lister) ListOpenPullRequests(ctx context.Context, repo providers.RepositoryRef) ([]providers.OpenPRSummary, error) {
	// ReserveCurrentPolls, not ReservePolls: retiring an expired window is the
	// scheduler's transition to report, and this poll must not consume it.
	if l.quota != nil {
		budget := l.quota.ReserveCurrentPolls(apiv1.Provider(repo.Provider), 1)
		if budget.Allowed == 0 {
			return l.shed(budget)
		}
		ctx = localscheduler.WithProviderPollBudget(ctx, budget)
	}
	access, done := accountFor(ctx, l.quota)
	defer done()
	provider, err := l.open(ctx, repo, access)
	if err != nil {
		return nil, fmt.Errorf("open provider for open-pr-list of %s/%s: %w", repo.Owner, repo.Name, err)
	}
	prs, err := provider.ListOpenPullRequests(ctx, repo)
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	l.last, l.at, l.have = append(l.last[:0], prs...), l.now(), true
	l.mu.Unlock()
	return prs, nil
}

func (l *lister) shed(budget localscheduler.ProviderPollBudget) ([]providers.OpenPRSummary, error) {
	shedErr := &localscheduler.ProviderPollBudgetError{Provider: budget.Provider, Remaining: budget.RemainingBefore, Requested: 1, ResetAt: budget.ResetAt}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.have {
		return nil, shedErr
	}
	deadline := l.at.Add(l.maxStale)
	if !budget.ResetAt.IsZero() && budget.ResetAt.Before(deadline) {
		deadline = budget.ResetAt
	}
	if l.now().After(deadline) {
		return nil, shedErr
	}
	return append([]providers.OpenPRSummary(nil), l.last...), nil
}

// accountFor builds the quota accounting of one poll. An admitted poll carries
// its prepaid reservation in ctx (the scheduler's, or the refresher's above);
// whatever it does not spend is refunded when the poll ends.
func accountFor(ctx context.Context, quota *localscheduler.ProviderQuotaState) (Access, func()) {
	if quota == nil {
		return Access{}, func() {}
	}
	accounting := &runtimewiring.ProviderQuotaAccounting{State: quota}
	if reservation, ok := localscheduler.ProviderPollReservationFromContext(ctx); ok {
		accounting.Prepaid = &reservation
	}
	return Access{Observer: accounting, Gate: accounting}, accounting.RefundUnused
}
