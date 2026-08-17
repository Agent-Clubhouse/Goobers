package main

import (
	"context"
	"fmt"
	"sort"
	"sync"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/fieldpredicate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/labelpredicate"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/providersnapshot"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/providers"
)

// newOpenPRProvider builds the GitHub client the open-PR lister polls; a package
// var so tests substitute a fake (mirrors newPRPoller / newEscalationPoster).
var newOpenPRProvider = func(token string, opts ...func(*providers.GitHubProvider)) localscheduler.OpenPRLister {
	return providers.NewGitHubProvider(token, opts...)
}

// resolvingOpenPRLister resolves the org-repo token per poll — honoring
// credentials.Resolver's re-read-on-resolve rotation contract, matching
// buildCIPollExecutor / the escalation notifier — registers it for scrubbing,
// and lists open PR heads through a freshly-authenticated provider. It is the
// OpenPRLister the #353 open-PR-count refresher polls off-tick.
type resolvingOpenPRLister struct {
	ref          string
	resolver     credentials.Resolver
	reg          runner.SecretRegistrar
	schedulerDir string
}

func (l *resolvingOpenPRLister) ListOpenPullRequests(ctx context.Context, repo providers.RepositoryRef) ([]providers.OpenPRSummary, error) {
	token, err := l.resolver.Resolve(ctx, l.ref)
	if err != nil {
		return nil, fmt.Errorf("resolve open-pr-list token for %s: %w", l.ref, err)
	}
	l.reg.Register([]byte(token))
	return newOpenPRProvider(token, apiReadCacheOptionForSnapshot(l.schedulerDir, "")).ListOpenPullRequests(ctx, repo)
}

// buildOpenPRRefresher constructs the #353 open-PR-count refreshers only when
// the instance actually needs them — a repo is configured AND some workflow
// opts into the MaxOpenPRs cap — so an instance that doesn't use the cap grows
// no GitHub poller and needs no token for it. Returns nil otherwise. One
// refresher is built per distinct repo among the capped workflows' gaggle
// projects (#2692), each listing through that repo's OWN owner/name token ref
// (the same binding credentials.RunnerGrants scopes the run path by): a
// gaggle's cap must bind on its own repo's PR count, never the first repo's.
// A gaggle whose project is zero or has no configured binding falls back to
// the first repo — byte-identical to RunnerGrants' first-binding default for
// such gaggles. Only the `up` daemon starts/wires the returned set; a single
// `goobers run` has no accretion to throttle. resolver is a fresh credential
// resolver over cfg (buildCredentials is read-only and idempotent), used only
// to authenticate the polls.
func buildOpenPRRefresher(cfg *instance.Config, workflows []apiv1.Workflow, gaggleProjects map[string]apiv1.RepoRef, reg runner.SecretRegistrar, branchNamespaces map[string]string, schedulerDir string, stores credentials.StoreResolver) (*localscheduler.OpenPRRefresherSet, error) {
	if len(cfg.Repos) == 0 {
		return nil, nil
	}
	cappedGaggles := make(map[string]bool)
	for i := range workflows {
		if workflows[i].Spec.Readiness.MaxOpenPRs > 0 {
			cappedGaggles[workflows[i].Spec.Gaggle] = true
		}
	}
	if len(cappedGaggles) == 0 {
		return nil, nil
	}
	resolver, _, err := buildCredentials(cfg, stores, "", "", nil, reg)
	if err != nil {
		return nil, fmt.Errorf("build open-pr-list credential resolver: %w", err)
	}
	byRepo := make(map[string]*localscheduler.OpenPRRefresher)
	byGaggle := make(map[string]*localscheduler.OpenPRRefresher, len(cappedGaggles))
	for gaggle := range cappedGaggles {
		repo := cfg.Repos[0]
		if configured, ok := configuredRepoForProject(cfg, gaggleProjects[gaggle]); ok {
			repo = configured
		} else if project := gaggleProjects[gaggle]; project.Owner != "" && project.Name != "" {
			// A project with no configured binding is polled under its own
			// owner/name ref; token resolution fails per-poll and the count
			// stays "unknown" (Admit fails open) instead of silently reading
			// the first repo's PRs.
			repo = instance.RepoRef{Owner: project.Owner, Name: project.Name, Provider: string(project.Provider)}
		}
		if repo.Provider == "ado" {
			// The cap counts GitHub PR heads; an ADO-projected gaggle has no
			// list to poll, so its count stays "unknown" (Admit fails open).
			continue
		}
		key := repo.Owner + "/" + repo.Name
		refresher := byRepo[key]
		if refresher == nil {
			lister := &resolvingOpenPRLister{ref: key, resolver: resolver, reg: reg, schedulerDir: schedulerDir}
			repoRef := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: repo.Owner, Name: repo.Name}
			// Exclude human-parked PRs from the cap (#986): goobers:merge-escalated is
			// the daemon's "parked pending a human" signal on a PR — it cannot be
			// drained autonomously, so counting it against MaxOpenPRs only starves new
			// implementation work. needs-remediation / blocked-on-sibling are
			// deliberately NOT excluded: the daemon can still drain those (remediation,
			// sibling sequencing), and the cap must keep applying backpressure to them.
			refresher = localscheduler.NewOpenPRRefresher(lister, repoRef, localscheduler.DefaultOpenPRRefreshInterval, []string{remediationEscalatedLabel}, branchNamespaces)
			byRepo[key] = refresher
		}
		byGaggle[gaggle] = refresher
	}
	if len(byGaggle) == 0 {
		return nil, nil
	}
	return localscheduler.NewOpenPRRefresherSet(byGaggle), nil
}

// backlogCounter adapts a provider + repo + label selector into a
// localscheduler.BacklogCounter (#344) — resolves its token per call (like
// escalationCommenter above), honoring credentials.Resolver's re-read-on-
// resolve rotation contract rather than capturing one at daemon startup.
type backlogCounter struct {
	mu             sync.Mutex
	ref            string
	repo           providers.RepositoryRef
	labels         []string
	labelPredicate *labelpredicate.Predicate
	fieldPredicate *fieldpredicate.Predicate
	resolver       credentials.Resolver
	reg            runner.SecretRegistrar
	schedulerDir   string
	quota          *localscheduler.ProviderQuotaState
	cursor         string
}

func (b *backlogCounter) EligibleCount(ctx context.Context) (int, error) {
	provider, cleanup, err := newCounterGitHubProvider(ctx, b.ref, b.schedulerDir, b.resolver, b.reg, b.quota)
	if err != nil {
		return 0, fmt.Errorf("resolve backlog-count token for %s: %w", b.ref, err)
	}
	defer cleanup()

	b.mu.Lock()
	cursor := b.cursor
	b.mu.Unlock()

	const pageSize = 100
	pageInfo := &providers.ListWorkItemsPageInfo{}
	items, err := provider.ListWorkItems(ctx, providers.ListWorkItemsRequest{
		Repository: b.repo, Labels: b.labels, State: "open", Limit: pageSize,
		Cursor: cursor, PageInfo: pageInfo, OldestFirst: true,
	})
	if err != nil {
		return 0, err
	}
	b.mu.Lock()
	if pageInfo.HasNext {
		b.cursor = pageInfo.NextCursor
	} else {
		b.cursor = ""
	}
	b.mu.Unlock()
	count := 0
	for _, item := range items {
		matched, err := b.labelPredicate.Matches(item.Labels)
		if err != nil {
			return 0, fmt.Errorf("evaluate backlog label predicate: %w", err)
		}
		if matched {
			matched, err = b.fieldPredicate.Matches(item.Fields)
			if err != nil {
				return 0, fmt.Errorf("evaluate backlog field predicate: %w", err)
			}
			if matched {
				count++
			}
		}
	}
	return count, nil
}

func newCounterGitHubProvider(
	ctx context.Context,
	ref string,
	schedulerDir string,
	resolver credentials.Resolver,
	reg runner.SecretRegistrar,
	quota *localscheduler.ProviderQuotaState,
) (*providers.GitHubProvider, func(), error) {
	var accounting *providerQuotaAccounting
	if quota != nil {
		accounting = &providerQuotaAccounting{state: quota}
		if reservation, ok := localscheduler.ProviderPollReservationFromContext(ctx); ok {
			accounting.prepaid = &reservation
		}
	}
	cleanup := func() {}
	if accounting != nil {
		cleanup = accounting.RefundUnused
	}

	token, err := resolver.Resolve(ctx, ref)
	if err != nil {
		cleanup()
		return nil, func() {}, err
	}
	reg.Register([]byte(token))
	opts := []func(*providers.GitHubProvider){
		apiReadCacheOptionForSnapshot(schedulerDir, providersnapshot.ID(ctx)),
	}
	if accounting != nil {
		opts = append(opts,
			providers.WithQuotaObserver(accounting),
			providers.WithQuotaRequestGate(accounting),
		)
	}
	// Fail fast on rate limits so polling waits for the scheduler's next
	// reset-aware admission. Transport and 5xx retries remain enabled and each
	// attempt is reserved through the quota gate above.
	opts = append(opts, providers.WithMaxRateLimitRetries(0))
	return newGitHubProvider(token, opts...), cleanup, nil
}

func (b *backlogCounter) ProviderQuotaGuarded() bool {
	return b.quota != nil
}

// buildBacklogCounter wires the daemon-side fan-out counter for a workflow's
// declared type=backlog-item trigger (#344). It counts work items carrying
// every selector key as a GitHub label. The per-run backlog-query stage remains
// the actual claiming mechanism; this only estimates how many runs a Tick
// should fan out to.
// Returns nil (not error) when wf declares no backlog-item trigger, or when
// no repo is configured — mirrors buildCIPollExecutor/buildEscalationNotifier's
// "irrelevant to this workflow" fail-open-to-nil shape, not a real error.
func buildBacklogCounter(cfg *instance.Config, gaggle apiv1.Gaggle, wf *apiv1.Workflow, repoRef apiv1.RepoRef, resolver credentials.Resolver, reg runner.SecretRegistrar, schedulerDir string, quota *localscheduler.ProviderQuotaState) (localscheduler.BacklogCounter, error) {
	if len(cfg.Repos) == 0 {
		return nil, nil
	}
	var selector map[string]string
	var expression string
	var fieldExpression string
	found := false
	for _, tr := range wf.Spec.Triggers {
		if tr.Type == apiv1.TriggerBacklogItem {
			selector = tr.Selector
			expression = tr.LabelPredicate
			fieldExpression = tr.FieldPredicate
			found = true
			break
		}
	}
	if !found {
		return nil, nil
	}
	labels := make([]string, 0, len(selector))
	for k := range selector {
		labels = append(labels, k)
	}
	sort.Strings(labels)
	predicate, err := labelpredicate.Compile(expression, labels, nil)
	if err != nil {
		return nil, fmt.Errorf("workflow %q backlog label predicate: %w", wf.Name, err)
	}
	fieldPredicate, err := fieldpredicate.CompileConjunction(gaggle.Spec.Backlog.FieldPredicate, fieldExpression)
	if err != nil {
		return nil, fmt.Errorf("workflow %q backlog field predicate: %w", wf.Name, err)
	}
	counter := &backlogCounter{
		// ref must follow the workflow's own repo (#2692 sibling): the query
		// below targets repoRef, so its token must resolve from the same
		// owner/name binding — matching buildScheduleDemandCounter.
		ref:            repoRef.Owner + "/" + repoRef.Name,
		repo:           providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: repoRef.Owner, Name: repoRef.Name},
		labels:         labels,
		labelPredicate: predicate,
		fieldPredicate: fieldPredicate,
		resolver:       resolver,
		reg:            reg,
		schedulerDir:   schedulerDir,
	}
	if quota != nil {
		counter.quota = quota
	}
	return counter, nil
}

// buildScheduleDemandCounter recognizes the built-in update-behind-pr selector
// and sizes each due schedule tick to its unclaimed eligible PR set.
func buildScheduleDemandCounter(
	cfg *instance.Config,
	wf *apiv1.Workflow,
	repoRef apiv1.RepoRef,
	resolver credentials.Resolver,
	reg runner.SecretRegistrar,
	schedulerDir, branchNamespace string,
	quota *localscheduler.ProviderQuotaState,
) localscheduler.BacklogCounter {
	if len(cfg.Repos) == 0 {
		return nil
	}
	hasSchedule := false
	for _, trigger := range wf.Spec.Triggers {
		if trigger.Type == apiv1.TriggerSchedule {
			hasSchedule = true
			break
		}
	}
	base, headPrefix, ok := remediationCounterScope(wf, branchNamespace)
	if !hasSchedule || !ok {
		return nil
	}
	return &remediationDemandCounter{
		ref:          repoRef.Owner + "/" + repoRef.Name,
		repo:         providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: repoRef.Owner, Name: repoRef.Name},
		base:         base,
		headPrefix:   headPrefix,
		gaggle:       wf.Spec.Gaggle,
		resolver:     resolver,
		reg:          reg,
		schedulerDir: schedulerDir,
		quota:        quota,
	}
}

func remediationCounterScope(wf *apiv1.Workflow, branchNamespace string) (base, headPrefix string, ok bool) {
	for _, task := range wf.Spec.Tasks {
		if task.Name != wf.Spec.Start || task.Run == nil ||
			len(task.Run.Command) != 2 ||
			task.Run.Command[0] != "goobers" ||
			task.Run.Command[1] != "update-behind-pr" {
			continue
		}
		base = task.Inputs["base"]
		if base == "" {
			base = "main"
		}
		headPrefix = task.Inputs["headPrefix"]
		if headPrefix == "" {
			headPrefix = providers.NormalizeBranchNamespace(branchNamespace)
		}
		return base, headPrefix, true
	}
	return "", "", false
}

type providerQuotaAccounting struct {
	mu          sync.Mutex
	state       *localscheduler.ProviderQuotaState
	prepaid     *localscheduler.ProviderPollReservation
	outstanding []localscheduler.ProviderPollReservation
}

func (a *providerQuotaAccounting) AcquireQuotaRequest(_ context.Context, provider providers.ProviderKind) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.prepaid != nil {
		a.outstanding = append(a.outstanding, *a.prepaid)
		a.prepaid = nil
		return nil
	}
	decision := a.state.ReserveCurrentPolls(apiv1.Provider(provider), 1)
	if decision.Allowed == 0 {
		return &localscheduler.ProviderPollBudgetError{
			Provider:  decision.Provider,
			Remaining: decision.RemainingBefore,
			Requested: 1,
			ResetAt:   decision.ResetAt,
		}
	}
	reservation, _ := decision.Reservation()
	a.outstanding = append(a.outstanding, reservation)
	return nil
}

func (a *providerQuotaAccounting) ObserveQuota(_ context.Context, observation providers.QuotaObservation) {
	a.mu.Lock()
	var reservation localscheduler.ProviderPollReservation
	if len(a.outstanding) > 0 {
		reservation = a.outstanding[0]
		a.outstanding = a.outstanding[1:]
	}
	a.mu.Unlock()

	provider := apiv1.Provider(observation.Provider)
	if observation.Cached {
		a.state.RefundReservation(reservation)
		return
	}
	if observation.Known {
		a.state.Record(provider, observation.Remaining, observation.Reset)
	}
}

func (a *providerQuotaAccounting) RefundUnused() {
	a.mu.Lock()
	if a.prepaid == nil {
		a.mu.Unlock()
		return
	}
	reservation := *a.prepaid
	a.prepaid = nil
	a.mu.Unlock()
	a.state.RefundReservation(reservation)
}
