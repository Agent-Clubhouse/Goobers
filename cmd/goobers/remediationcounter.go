package main

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/providers"
)

type remediationDemandCounter struct {
	ref          string
	repo         providers.RepositoryRef
	base         string
	headPrefix   string
	gaggle       string
	resolver     credentials.Resolver
	reg          runner.SecretRegistrar
	schedulerDir string
	quota        *localscheduler.ProviderQuotaState
	now          func() time.Time
}

func (c *remediationDemandCounter) EligibleCount(ctx context.Context) (int, error) {
	provider, cleanup, err := newCounterGitHubProvider(ctx, c.ref, c.schedulerDir, c.resolver, c.reg, c.quota)
	if err != nil {
		return 0, fmt.Errorf("resolve remediation-count token for %s: %w", c.ref, err)
	}
	defer cleanup()

	prs, err := provider.ListPullRequests(ctx, providers.ListPullRequestsRequest{
		Repository: c.repo,
		Base:       c.base,
		HeadPrefix: c.headPrefix,
	})
	if err != nil {
		return 0, err
	}
	prs, blockedDependents, err := filterRemediationPullRequests(ctx, provider, c.repo, prs, nil)
	if err != nil {
		return 0, err
	}
	now := time.Now()
	if c.now != nil {
		now = c.now()
	}
	prs, err = filterClaimAvailablePullRequests(c.schedulerDir, c.gaggle, c.repo.Provider, "", prs, now)
	if err != nil {
		return 0, err
	}
	baseTips := map[string]string{}
	candidates, _, err := selectRemediationCandidates(prs, blockedDependents, func(pr providers.PullRequestSummary) (bool, error) {
		return pullRequestBehindLiveBase(ctx, provider, c.repo, pr, baseTips)
	})
	if err != nil {
		return 0, err
	}
	return len(candidates), nil
}

func (c *remediationDemandCounter) ProviderQuotaGuarded() bool {
	return c.quota != nil
}

func filterClaimAvailablePullRequests(
	schedulerDir string,
	gaggle string,
	provider providers.ProviderKind,
	currentRunID string,
	candidates []providers.PullRequestSummary,
	now time.Time,
) ([]providers.PullRequestSummary, error) {
	available := make([]providers.PullRequestSummary, 0, len(candidates))
	err := withClaimLock(filepath.Join(schedulerDir, claimLockFileName), claimLockOperationPRCount, func() error {
		ledger, err := localscheduler.OpenClaimLedger(filepath.Join(schedulerDir, claimLedgerFileName))
		if err != nil {
			return fmt.Errorf("open claim ledger: %w", err)
		}
		for _, candidate := range candidates {
			claimed, ownedByCurrentRun := pullRequestClaimStatus(ledger, gaggle, provider, candidate.Number, currentRunID, now)
			if !claimed || ownedByCurrentRun {
				available = append(available, candidate)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return available, nil
}
