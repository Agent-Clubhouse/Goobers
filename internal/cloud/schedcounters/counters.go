// Package schedcounters supplies the provider-backed demand counters and the
// open-PR cap counter the cloud control role's scheduler entries need (L1d).
// It ports the pure, runner-free half of the local daemon's counter wiring: the
// local counters are built in package main around a worktree-and-scheduler-dir
// world (read cache, quota file, claim ledger file) that the cloud does not
// have, so what is shared here is the selection logic, and everything that
// touches a forge goes through one injected seam.
//
// Credentials never enter this package. Deps.Open returns a ready provider for a
// repository, resolving (and scrubbing) the token on every call so rotation
// takes effect; the control composition binds it to the applied instance config.
//
// Not yet supplied: the schedule-demand counter (scheduled update-behind-pr
// remediation). It needs the cloud claims ledger and live-origin filters, so the
// entry keeps its ScheduleDemandPendingReason until it lands.
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
	"github.com/goobers/goobers/internal/runtimewiring"
	"github.com/goobers/goobers/providers"
)

// Provider is the forge surface the counters read.
type Provider interface {
	ListWorkItems(ctx context.Context, req providers.ListWorkItemsRequest) ([]providers.WorkItem, error)
	ListOpenPullRequests(ctx context.Context, repo providers.RepositoryRef) ([]providers.OpenPRSummary, error)
}

// Deps binds one applied generation's counters.
type Deps struct {
	// Config and Set are the applied generation's policy and definitions (the same
	// pair schedentries.Input carries).
	Config *instance.Config
	Set    *instance.ConfigSet
	// Open returns a provider for repo with freshly resolved credentials. It is
	// called once per poll, never cached, so a rotated token is picked up.
	Open func(ctx context.Context, repo providers.RepositoryRef) (Provider, error)
	// SelfIdentity is the login a respectAssignee backlog-query matches.
	SelfIdentity string
	// OpenPRInterval overrides localscheduler.DefaultOpenPRRefreshInterval.
	OpenPRInterval time.Duration
	// Report receives refresh failures that leave a count unknown. May be nil.
	Report func(error)
}

// Counters are one generation's counters. It is safe for concurrent use.
type Counters struct {
	deps    Deps
	gaggles map[string]apiv1.Gaggle
	open    *localscheduler.OpenPRRefresherSet

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
	open, err := c.buildOpenPRs()
	if err != nil {
		return nil, err
	}
	c.open = open
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
// admission fails open) until a refresher's first poll completes, and always for
// a gaggle with no refresher.
func (c *Counters) OpenPRs() localscheduler.OpenPRCounter { return c.open }

// Run polls the open-PR refreshers until ctx ends and then returns. The caller
// binds ctx to the leadership epoch, so a deposed controller stops polling and a
// new one starts again from "unknown".
func (c *Counters) Run(ctx context.Context) error {
	if c.open != nil {
		c.open.Run(ctx)
	}
	<-ctx.Done()
	return nil
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
				&lister{open: c.deps.Open, report: c.deps.Report}, repo, c.deps.OpenPRInterval,
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

// lister adapts the per-call opener to the refresher's lister.
type lister struct {
	open   func(context.Context, providers.RepositoryRef) (Provider, error)
	report func(error)
}

func (l *lister) ListOpenPullRequests(ctx context.Context, repo providers.RepositoryRef) ([]providers.OpenPRSummary, error) {
	provider, err := l.open(ctx, repo)
	if err != nil {
		return nil, fmt.Errorf("open provider for open-pr-list of %s/%s: %w", repo.Owner, repo.Name, err)
	}
	return provider.ListOpenPullRequests(ctx, repo)
}
