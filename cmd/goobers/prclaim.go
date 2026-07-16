package main

import (
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

const pullRequestClaimPrefix = "pr/"

func pullRequestClaimKey(number int) string {
	return pullRequestClaimPrefix + strconv.Itoa(number)
}

func claimEligiblePullRequest(root string, eligible []providers.PullRequestSummary) (*providers.PullRequestSummary, error) {
	runID, workflow, err := providerRunContext()
	if err != nil {
		return nil, err
	}
	leaseDuration, err := pullRequestClaimLease()
	if err != nil {
		return nil, err
	}
	return claimPullRequest(root, eligible, runID, workflow, leaseDuration)
}

func pullRequestClaimLease() (time.Duration, error) {
	value := providerInput("leaseDuration", "")
	if value == "" {
		return DefaultClaimLease, nil
	}
	leaseDuration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("invalid leaseDuration %q: %w", value, err)
	}
	if leaseDuration <= 0 {
		return 0, fmt.Errorf("invalid leaseDuration %q: must be positive", value)
	}
	return leaseDuration, nil
}

func claimPullRequest(
	root string,
	eligible []providers.PullRequestSummary,
	runID, workflow string,
	leaseDuration time.Duration,
) (*providers.PullRequestSummary, error) {
	candidates := append([]providers.PullRequestSummary(nil), eligible...)
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Number < candidates[j].Number
	})

	l := layoutFor(root)
	var selected *providers.PullRequestSummary
	err := withClaimLock(filepath.Join(l.SchedulerDir(), claimLockFileName), func() error {
		instanceLog, _, err := journal.OpenInstanceLog(l.SchedulerDir())
		if err != nil {
			return fmt.Errorf("open instance log: %w", err)
		}
		defer func() { _ = instanceLog.Close() }()

		ledger, err := localscheduler.OpenClaimLedger(
			filepath.Join(l.SchedulerDir(), claimLedgerFileName),
			localscheduler.WithInstanceLog(instanceLog),
		)
		if err != nil {
			return fmt.Errorf("open claim ledger: %w", err)
		}
		for _, candidate := range candidates {
			ok, _, err := ledger.Claim(
				pullRequestClaimKey(candidate.Number),
				runID,
				workflow,
				leaseDuration,
			)
			if err != nil {
				return fmt.Errorf("claim PR #%d in ledger: %w", candidate.Number, err)
			}
			if ok {
				candidate := candidate
				selected = &candidate
				break
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return selected, nil
}
