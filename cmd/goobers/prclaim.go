package main

import (
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
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

// claimedPullRequestNumber recovers the PR number THIS run claimed, from the
// durable claim ledger rather than from a threaded stage input (issue #392).
//
// Task.InputsFrom only resolves against the immediately preceding TASK's own
// Outputs, so a stage sitting after pr-remediation's agentic chain cannot
// receive gather-pr-context's selectedNumber: `implement` (a goober session
// whose result is status + summary only) and `local-ci` (`make ci`) each
// become the upstream in turn and neither carries it. issue-close-out already
// solves the identical problem the identical way (issuecloseout.go's
// ForRun lookup, #241) — the ledger entry this run took in
// gather-pr-context is the run-scoped durable state that outlives the
// InputsFrom chain, and outlives a crash/resume with it.
//
// Returns ok=false when this run holds no PR claim at all — for a caller
// reached only via gather-pr-context having claimed one, that means a prior
// attempt of a later stage already released it, which callers treat as an
// idempotent no-op rather than an error (same contract as close-out's).
func claimedPullRequestNumber(root string) (number int, ok bool, err error) {
	runID, _, err := providerRunContext()
	if err != nil {
		return 0, false, err
	}
	l := layoutFor(root)
	var claimed string
	lockErr := withClaimLock(filepath.Join(l.SchedulerDir(), claimLockFileName), claimLockOperationPRLookup, func() error {
		ledger, lerr := localscheduler.OpenClaimLedger(filepath.Join(l.SchedulerDir(), claimLedgerFileName))
		if lerr != nil {
			return fmt.Errorf("open claim ledger: %w", lerr)
		}
		for _, entry := range ledger.ForRunAll(runID) {
			if strings.HasPrefix(entry.ItemID, pullRequestClaimPrefix) {
				claimed = strings.TrimPrefix(entry.ItemID, pullRequestClaimPrefix)
				break
			}
		}
		return nil
	})
	if lockErr != nil {
		return 0, false, lockErr
	}
	if claimed == "" {
		return 0, false, nil
	}
	// ForRunAll is prefix-filtered above, so this can only fail on a ledger
	// somebody hand-edited — surfaced rather than silently treated as "no
	// claim", which would let a caller push to the wrong PR's branch.
	number, perr := strconv.Atoi(claimed)
	if perr != nil {
		return 0, false, fmt.Errorf("claim ledger holds malformed PR claim %q for run %s: %w", claimed, runID, perr)
	}
	return number, true, nil
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
	err := withClaimLock(filepath.Join(l.SchedulerDir(), claimLockFileName), claimLockOperationPRAcquire, func() error {
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
