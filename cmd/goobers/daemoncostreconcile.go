package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

var mergedPRCostSweepInterval = 15 * time.Minute

const (
	mergedPRCostSweepLookback = 7 * 24 * time.Hour
	mergedPRCostSweepBatch    = 100
)

var errMergedPRCostSweepAlreadyRunning = errors.New("merged pull request cost sweep already running")

type mergedPRCostSweepGate struct {
	mu      sync.Mutex
	running bool
}

func (g *mergedPRCostSweepGate) run(fn func() error) error {
	g.mu.Lock()
	if g.running {
		g.mu.Unlock()
		return errMergedPRCostSweepAlreadyRunning
	}
	g.running = true
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		g.running = false
		g.mu.Unlock()
	}()
	return fn()
}

func startDeferredMergedPRCostSweep(
	ctx context.Context,
	gate *mergedPRCostSweepGate,
	reporter *sweepErrorReporter,
	sweep func(time.Time) error,
	ready bool,
) <-chan struct{} {
	done := make(chan struct{})
	if !ready {
		close(done)
		return done
	}
	go func() {
		defer close(done)
		err := gate.run(func() error { return sweep(time.Now()) })
		if !errors.Is(err, errMergedPRCostSweepAlreadyRunning) {
			reporter.report(err)
		}
	}()
	return done
}

type recentlyMergedCostProvider interface {
	issueCommentCostProvider
	ListRecentlyClosedPullRequests(context.Context, providers.ListPullRequestsRequest, time.Time) ([]providers.PullRequestSummary, error)
}

type mergedPRCostSweepReport struct {
	Repositories   int
	Scanned        int
	Eligible       int
	Updated        int
	QuerySucceeded bool
	NextPage       int
}

type daemonMergedPRCostTarget struct {
	Project         apiv1.RepoRef
	Repository      instance.RepoRef
	BranchNamespace string
}

type daemonMergedPRCostReconciler struct {
	cfg       *instance.Config
	stores    credentials.StoreResolver
	registrar credentials.SecretRegistrar
	quota     *localscheduler.ProviderQuotaState

	mu    sync.RWMutex
	set   *instance.ConfigSet
	pages map[string]int
}

func newDaemonMergedPRCostReconciler(
	cfg *instance.Config,
	set *instance.ConfigSet,
	stores credentials.StoreResolver,
	registrar credentials.SecretRegistrar,
	quota *localscheduler.ProviderQuotaState,
) *daemonMergedPRCostReconciler {
	return &daemonMergedPRCostReconciler{
		cfg: cfg, set: set, stores: stores, registrar: registrar, quota: quota, pages: map[string]int{},
	}
}

func (r *daemonMergedPRCostReconciler) Replace(set *instance.ConfigSet) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.set = set
	r.mu.Unlock()
}

func (r *daemonMergedPRCostReconciler) Sweep(ctx context.Context, now time.Time) (mergedPRCostSweepReport, error) {
	var report mergedPRCostSweepReport
	if r == nil || r.cfg == nil {
		return report, nil
	}
	targets, err := r.targets()
	if err != nil {
		return report, err
	}
	var errs []error
	for _, target := range targets {
		targetKey := daemonMergedPRCostTargetKey(target)
		prProvider, issueProvider, repo, err := r.providers(ctx, target)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", target.Repository.Owner+"/"+target.Repository.Name, err))
			continue
		}
		if prProvider == nil {
			continue
		}
		report.Repositories++
		targetReport, targetErr := reconcileRecentlyMergedPRCosts(
			ctx,
			prProvider,
			issueProvider,
			repo,
			target.BranchNamespace,
			r.cfg.DaemonIdentity != nil && repo.Provider == providers.ProviderGitHub,
			now.UTC().Add(-mergedPRCostSweepLookback),
			mergedPRCostSweepBatch,
			r.page(targetKey),
		)
		report.Scanned += targetReport.Scanned
		report.Eligible += targetReport.Eligible
		report.Updated += targetReport.Updated
		if targetReport.QuerySucceeded {
			r.setPage(targetKey, targetReport.NextPage)
		}
		if targetErr != nil {
			errs = append(errs, fmt.Errorf("%s: %w", repo.CanonicalKey(), targetErr))
		}
	}
	return report, errors.Join(errs...)
}

func (r *daemonMergedPRCostReconciler) page(key string) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	page := r.pages[key]
	if page < 1 {
		return 1
	}
	return page
}

func (r *daemonMergedPRCostReconciler) setPage(key string, page int) {
	if page < 1 {
		page = 1
	}
	r.mu.Lock()
	r.pages[key] = page
	r.mu.Unlock()
}

func (r *daemonMergedPRCostReconciler) targets() ([]daemonMergedPRCostTarget, error) {
	r.mu.RLock()
	set := r.set
	r.mu.RUnlock()

	namespaces := map[string]string{}
	if set != nil {
		namespaces = branchNamespacesByGaggle(set)
	}
	targets := make(map[string]daemonMergedPRCostTarget)
	representedRepos := make(map[string]bool)
	if set != nil {
		for i := range set.Gaggles {
			gaggle := set.Gaggles[i]
			repo, ok := configuredRepoForProject(r.cfg, gaggle.Spec.Project)
			if !ok {
				if gaggle.Spec.Project.Owner == "" || gaggle.Spec.Project.Name == "" {
					continue
				}
				return nil, fmt.Errorf("gaggle %q project %s/%s is not configured", gaggle.Name, gaggle.Spec.Project.Owner, gaggle.Spec.Project.Name)
			}
			target := daemonMergedPRCostTarget{
				Project:         gaggle.Spec.Project,
				Repository:      repo,
				BranchNamespace: namespaces[gaggle.Name],
			}
			targets[daemonMergedPRCostTargetKey(target)] = target
			representedRepos[daemonMergedPRCostRepositoryKey(repo)] = true
		}
	}
	for _, repo := range r.cfg.Repos {
		if representedRepos[daemonMergedPRCostRepositoryKey(repo)] {
			continue
		}
		target := daemonMergedPRCostTarget{
			Project: apiv1.RepoRef{
				Provider: apiv1.Provider(repo.Provider),
				BaseURL:  repo.BaseURL,
				Owner:    repo.Owner,
				Project:  repo.Project,
				Name:     repo.Name,
			},
			Repository:      repo,
			BranchNamespace: providers.DefaultBranchNamespace,
		}
		key := daemonMergedPRCostTargetKey(target)
		if _, exists := targets[key]; !exists {
			targets[key] = target
		}
	}

	keys := make([]string, 0, len(targets))
	for key := range targets {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]daemonMergedPRCostTarget, 0, len(keys))
	for _, key := range keys {
		out = append(out, targets[key])
	}
	return out, nil
}

func daemonMergedPRCostTargetKey(target daemonMergedPRCostTarget) string {
	return daemonMergedPRCostRepositoryKey(target.Repository) + "#" +
		providers.NormalizeBranchNamespace(target.BranchNamespace)
}

func daemonMergedPRCostRepositoryKey(repository instance.RepoRef) string {
	repo := providers.RepositoryRef{
		Provider: providers.ProviderKind(repository.Provider),
		Owner:    repository.Owner,
		Project:  repository.Project,
		Name:     repository.Name,
		URL:      repository.BaseURL,
	}
	return repo.CanonicalKey()
}

func (r *daemonMergedPRCostReconciler) providers(
	ctx context.Context,
	target daemonMergedPRCostTarget,
) (recentlyMergedCostProvider, issueCommentCostProvider, providers.RepositoryRef, error) {
	repo := providers.RepositoryRef{
		Provider: providers.ProviderKind(target.Repository.Provider),
		Owner:    target.Repository.Owner,
		Project:  target.Repository.Project,
		Name:     target.Repository.Name,
		URL:      target.Repository.BaseURL,
	}
	if repo.Provider == providers.ProviderADO || repo.Provider == providers.ProviderGitea {
		return nil, nil, repo, nil
	}
	if repo.Provider != providers.ProviderGitHub {
		return nil, nil, repo, fmt.Errorf("unsupported provider %q", repo.Provider)
	}

	resolver, grants, err := buildCredentials(
		r.cfg,
		r.stores,
		target.Project.Owner,
		target.Project.Name,
		nil,
		r.registrar,
	)
	if err != nil {
		return nil, nil, repo, fmt.Errorf("build credentials: %w", err)
	}
	injector, err := credentials.NewInjector(resolver, grants, r.registrar)
	if err != nil {
		return nil, nil, repo, fmt.Errorf("build credential injector: %w", err)
	}
	set, err := injector.Materialize(ctx, []string{
		string(capability.GitHubPRWrite),
		string(capability.GitHubIssuesWrite),
	})
	if err != nil {
		return nil, nil, repo, fmt.Errorf("materialize credentials: %w", err)
	}
	prToken, err := set.Token(ctx, string(capability.GitHubPRWrite))
	if err != nil {
		return nil, nil, repo, err
	}
	issueToken, err := set.Token(ctx, string(capability.GitHubIssuesWrite))
	if err != nil {
		return nil, nil, repo, err
	}

	login := r.cfg.GitHubBotLogin(repo.Owner, repo.Name)
	return newGitHubProvider(prToken, daemonGitHubProviderOptions(login, r.quota)...),
		newGitHubProvider(issueToken, daemonGitHubProviderOptions(login, r.quota)...),
		repo,
		nil
}

func daemonGitHubProviderOptions(
	login string,
	quota *localscheduler.ProviderQuotaState,
) []func(*providers.GitHubProvider) {
	var opts []func(*providers.GitHubProvider)
	if login != "" {
		opts = append(opts, providers.WithConfiguredLogin(login))
	}
	if quota != nil {
		accounting := &providerQuotaAccounting{state: quota}
		opts = append(opts,
			providers.WithQuotaRequestGate(accounting),
			providers.WithQuotaObserver(accounting),
		)
	}
	return opts
}

func reconcileRecentlyMergedPRCosts(
	ctx context.Context,
	prProvider recentlyMergedCostProvider,
	issueProvider issueCommentCostProvider,
	repo providers.RepositoryRef,
	branchNamespace string,
	useAuthenticatedAuthorForOwnership bool,
	updatedSince time.Time,
	limit int,
	page int,
) (mergedPRCostSweepReport, error) {
	var report mergedPRCostSweepReport
	prAuthor, err := prProvider.AuthenticatedLogin(ctx)
	if err != nil {
		return report, fmt.Errorf("resolve pull request cost receipt author: %w", err)
	}
	issueAuthor, issueAuthorErr := issueProvider.AuthenticatedLogin(ctx)
	prs, err := prProvider.ListRecentlyClosedPullRequests(ctx, providers.ListPullRequestsRequest{
		Repository:     repo,
		Limit:          limit,
		Page:           page,
		SkipCheckState: true,
	}, updatedSince)
	if err != nil {
		return report, fmt.Errorf("list recently closed pull requests: %w", err)
	}
	report.QuerySucceeded = true
	report.NextPage = 1
	if limit > 0 && len(prs) >= limit {
		report.NextPage = page + 1
	}

	var errs []error
	if issueAuthorErr != nil {
		errs = append(errs, fmt.Errorf("resolve issue cost receipt author: %w", issueAuthorErr))
	}
	expectedAuthor := ""
	if useAuthenticatedAuthorForOwnership {
		expectedAuthor = prAuthor
	}
	namespace := providers.NormalizeBranchNamespace(branchNamespace)
	for _, pr := range prs {
		report.Scanned++
		if !pr.Merged || !isOwnPullRequest(pr.Author, pr.Head, []string{namespace}, expectedAuthor) {
			continue
		}
		report.Eligible++
		_, updated, reconcileErr := reconcileIssueCommentCostSummary(
			ctx,
			prProvider,
			issueProvider,
			repo,
			strconv.Itoa(pr.Number),
			closingIssueNumbers(pr.Body),
			prAuthor,
			issueAuthor,
		)
		if updated {
			report.Updated++
		}
		if reconcileErr != nil {
			errs = append(errs, fmt.Errorf("pull request #%d: %w", pr.Number, reconcileErr))
		}
	}
	return report, errors.Join(errs...)
}
