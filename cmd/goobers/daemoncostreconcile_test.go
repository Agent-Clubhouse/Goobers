package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

type fakeRecentlyMergedCostProvider struct {
	login        string
	prs          []providers.PullRequestSummary
	comments     map[string][]providers.Comment
	updatedSince time.Time
	limit        int
	page         int
	updates      []providers.UpdateWorkItemRequest
}

func (p *fakeRecentlyMergedCostProvider) AuthenticatedLogin(context.Context) (string, error) {
	return p.login, nil
}

func (p *fakeRecentlyMergedCostProvider) ListRecentlyClosedPullRequests(
	_ context.Context,
	req providers.ListPullRequestsRequest,
	updatedSince time.Time,
) ([]providers.PullRequestSummary, error) {
	p.updatedSince = updatedSince
	p.limit = req.Limit
	p.page = req.Page
	return append([]providers.PullRequestSummary(nil), p.prs...), nil
}

func (p *fakeRecentlyMergedCostProvider) ListComments(
	_ context.Context,
	_ providers.RepositoryRef,
	id string,
) ([]providers.Comment, error) {
	return append([]providers.Comment(nil), p.comments[id]...), nil
}

func (p *fakeRecentlyMergedCostProvider) UpdateWorkItem(
	_ context.Context,
	req providers.UpdateWorkItemRequest,
) (providers.WorkItem, error) {
	p.updates = append(p.updates, req)
	p.comments[req.ID] = append(p.comments[req.ID], providers.Comment{
		Author: p.login,
		Body:   req.Comment,
	})
	return providers.WorkItem{ID: req.ID}, nil
}

func TestReconcileRecentlyMergedPRCostsIsBoundedOwnedAndIdempotent(t *testing.T) {
	prProvider := &fakeRecentlyMergedCostProvider{
		login: "goobers[bot]",
		prs: []providers.PullRequestSummary{
			{Number: 20, Author: "goobers[bot]", Head: "goobers/implementation/run-20", Merged: true, Body: "Fixes #42"},
			{Number: 21, Author: "human", Head: "human/fix", Merged: true},
			{Number: 22, Author: "goobers[bot]", Head: "goobers/implementation/run-22", Merged: false},
		},
		comments: map[string][]providers.Comment{
			"20": {costComment(t, "goobers[bot]", "merge-review", "run-review", 20, 2_000_000_000)},
		},
	}
	issueProvider := &fakeRecentlyMergedCostProvider{
		login: "goobers[bot]",
		comments: map[string][]providers.Comment{
			"42": {costComment(t, "goobers[bot]", "implementation", "run-implementation", 10, 4_000_000_000)},
		},
	}
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"}
	updatedSince := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

	report, err := reconcileRecentlyMergedPRCosts(
		context.Background(),
		prProvider,
		issueProvider,
		repo,
		"goobers/",
		true,
		updatedSince,
		25,
		1,
	)
	if err != nil {
		t.Fatalf("reconcileRecentlyMergedPRCosts: %v", err)
	}
	if report.Scanned != 3 || report.Eligible != 1 || report.Updated != 1 {
		t.Fatalf("report = %+v, want scanned=3 eligible=1 updated=1", report)
	}
	if !prProvider.updatedSince.Equal(updatedSince) || prProvider.limit != 25 || prProvider.page != 1 {
		t.Fatalf("list bounds = since %s limit %d page %d, want %s, 25, 1", prProvider.updatedSince, prProvider.limit, prProvider.page, updatedSince)
	}
	if len(prProvider.updates) != 1 || prProvider.updates[0].ID != "20" {
		t.Fatalf("updates = %+v, want only PR #20", prProvider.updates)
	}
	if !strings.Contains(prProvider.updates[0].Comment, "Your cost for this PR was **6.00 AIC**") {
		t.Fatalf("summary = %q, want 6.00 AIC", prProvider.updates[0].Comment)
	}
	prProvider.comments["20"][len(prProvider.comments["20"])-1].Body += "\n\nPosted by **Goobers**"

	report, err = reconcileRecentlyMergedPRCosts(
		context.Background(),
		prProvider,
		issueProvider,
		repo,
		"goobers/",
		true,
		updatedSince,
		25,
		1,
	)
	if err != nil {
		t.Fatalf("second reconcileRecentlyMergedPRCosts: %v", err)
	}
	if report.Updated != 0 || len(prProvider.updates) != 1 {
		t.Fatalf("attributed reconciliation = %+v updates=%d, want no duplicate", report, len(prProvider.updates))
	}

	prProvider.comments["20"] = append(prProvider.comments["20"],
		costComment(t, "goobers[bot]", "pr-remediation", "run-remediation", 30, 1_000_000_000))
	report, err = reconcileRecentlyMergedPRCosts(
		context.Background(),
		prProvider,
		issueProvider,
		repo,
		"goobers/",
		true,
		updatedSince,
		25,
		1,
	)
	if err != nil {
		t.Fatalf("refreshed reconcileRecentlyMergedPRCosts: %v", err)
	}
	if report.Updated != 0 || len(prProvider.updates) != 1 {
		t.Fatalf("receipt-changed reconciliation = %+v updates=%d, want existing summary preserved", report, len(prProvider.updates))
	}
}

func TestReconcileRecentlyMergedPRCostsUsesBranchOwnershipWithoutDaemonIdentity(t *testing.T) {
	provider := &fakeRecentlyMergedCostProvider{
		login: "operator",
		prs: []providers.PullRequestSummary{
			{Number: 40, Author: "operator", Head: "goobers/implementation/run-40", Merged: true},
			{Number: 41, Author: "operator", Head: "human/fix", Merged: true},
		},
		comments: map[string][]providers.Comment{
			"40": {costComment(t, "operator", "implementation", "run-40", 10, 3_000_000_000)},
			"41": {costComment(t, "operator", "implementation", "run-41", 10, 9_000_000_000)},
		},
	}
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"}

	report, err := reconcileRecentlyMergedPRCosts(
		context.Background(),
		provider,
		provider,
		repo,
		"goobers/",
		false,
		time.Now().Add(-time.Hour),
		10,
		1,
	)
	if err != nil {
		t.Fatalf("reconcileRecentlyMergedPRCosts: %v", err)
	}
	if report.Eligible != 1 || report.Updated != 1 || len(provider.updates) != 1 || provider.updates[0].ID != "40" {
		t.Fatalf("report=%+v updates=%+v, want only namespaced PR #40", report, provider.updates)
	}
}

func TestDaemonGitHubProviderOptionsRespectSharedQuota(t *testing.T) {
	quota := localscheduler.NewProviderQuotaState()
	resetAt := time.Now().Add(time.Hour)
	quota.Record(apiv1.ProviderGitHub, 0, resetAt)
	provider := providers.NewGitHubProvider("token", daemonGitHubProviderOptions("", quota)...)

	_, err := provider.AuthenticatedLogin(context.Background())
	var budgetErr *localscheduler.ProviderPollBudgetError
	if !errors.As(err, &budgetErr) {
		t.Fatalf("AuthenticatedLogin error = %v, want ProviderPollBudgetError", err)
	}
	if budgetErr.Provider != apiv1.ProviderGitHub || !budgetErr.ResetAt.Equal(resetAt) {
		t.Fatalf("budget error = %+v, want GitHub reset at %s", budgetErr, resetAt)
	}
}

func TestDaemonMergedPRCostProvidersSkipUnsupportedBackstops(t *testing.T) {
	reconciler := &daemonMergedPRCostReconciler{}
	for _, provider := range []string{string(providers.ProviderADO), string(providers.ProviderGitea)} {
		target := daemonMergedPRCostTarget{Repository: instance.RepoRef{
			Provider: provider,
			Owner:    "your-org",
			Project:  "your-project",
			Name:     "your-repo",
		}}
		prProvider, issueProvider, _, err := reconciler.providers(context.Background(), target)
		if err != nil {
			t.Fatalf("%s providers: %v", provider, err)
		}
		if prProvider != nil || issueProvider != nil {
			t.Fatalf("%s providers = %T, %T, want skipped nil providers", provider, prProvider, issueProvider)
		}
	}
}

func TestMergedPRCostSweepGateCoalescesConcurrentRuns(t *testing.T) {
	gate := &mergedPRCostSweepGate{}
	inFlight := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- gate.run(func() error {
			close(inFlight)
			<-release
			return nil
		})
	}()

	select {
	case <-inFlight:
	case <-time.After(2 * time.Second):
		t.Fatal("first sweep never started")
	}
	if err := gate.run(func() error {
		t.Fatal("second sweep ran concurrently")
		return nil
	}); !errors.Is(err, errMergedPRCostSweepAlreadyRunning) {
		t.Fatalf("concurrent gate.run = %v, want errMergedPRCostSweepAlreadyRunning", err)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first sweep returned %v", err)
	}
}

func TestStartDeferredMergedPRCostSweepHonorsReadiness(t *testing.T) {
	gate := &mergedPRCostSweepGate{}
	reporter := newSweepErrorReporter(nil, "test")
	calls := 0
	sweep := func(time.Time) error {
		calls++
		return nil
	}

	<-startDeferredMergedPRCostSweep(context.Background(), gate, reporter, sweep, false)
	if calls != 0 {
		t.Fatalf("not-ready startup calls = %d, want 0", calls)
	}
	<-startDeferredMergedPRCostSweep(context.Background(), gate, reporter, sweep, true)
	if calls != 1 {
		t.Fatalf("ready startup calls = %d, want 1", calls)
	}
}

func TestReconcileRecentlyMergedPRCostsAdvancesAndWrapsPage(t *testing.T) {
	provider := &fakeRecentlyMergedCostProvider{
		login: "goobers[bot]",
		prs: []providers.PullRequestSummary{
			{Number: 50, Author: "human", Merged: true},
			{Number: 51, Author: "human", Merged: true},
		},
		comments: map[string][]providers.Comment{},
	}
	repo := providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "your-org", Name: "your-repo"}

	report, err := reconcileRecentlyMergedPRCosts(
		context.Background(), provider, provider, repo, "goobers/", true, time.Now().Add(-time.Hour), 2, 3,
	)
	if err != nil {
		t.Fatalf("full page reconcile: %v", err)
	}
	if report.NextPage != 4 {
		t.Fatalf("full page next = %d, want 4", report.NextPage)
	}

	provider.prs = provider.prs[:1]
	report, err = reconcileRecentlyMergedPRCosts(
		context.Background(), provider, provider, repo, "goobers/", true, time.Now().Add(-time.Hour), 2, 4,
	)
	if err != nil {
		t.Fatalf("short page reconcile: %v", err)
	}
	if report.NextPage != 1 {
		t.Fatalf("short page next = %d, want 1", report.NextPage)
	}
}

func TestDaemonMergedPRCostTargetsKeepDistinctNamespacesAndReplaceDefinitions(t *testing.T) {
	cfg := &instance.Config{Repos: []instance.RepoRef{
		{Provider: string(providers.ProviderGitHub), Owner: "your-org", Name: "your-repo"},
	}}
	project := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "your-org", Name: "your-repo"}
	set := &instance.ConfigSet{Gaggles: []apiv1.Gaggle{
		{ObjectMeta: metav1.ObjectMeta{Name: "alpha"}, Spec: apiv1.GaggleSpec{Project: project, BranchNamespace: "alpha"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "beta"}, Spec: apiv1.GaggleSpec{Project: project, BranchNamespace: "beta/"}},
	}}
	reconciler := newDaemonMergedPRCostReconciler(cfg, set, nil, nil, nil)

	targets, err := reconciler.targets()
	if err != nil {
		t.Fatalf("targets: %v", err)
	}
	if len(targets) != 2 || targets[0].BranchNamespace != "alpha/" || targets[1].BranchNamespace != "beta/" {
		t.Fatalf("targets = %+v, want distinct normalized alpha/beta namespaces", targets)
	}

	reconciler.Replace(&instance.ConfigSet{})
	targets, err = reconciler.targets()
	if err != nil {
		t.Fatalf("targets after replace: %v", err)
	}
	if len(targets) != 1 || targets[0].BranchNamespace != providers.DefaultBranchNamespace {
		t.Fatalf("targets after replace = %+v, want configured repo with default namespace", targets)
	}
}
