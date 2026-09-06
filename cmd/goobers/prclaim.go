package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

const pullRequestClaimPrefix = "pr/"

func pullRequestClaimKey(number int) string {
	return pullRequestClaimPrefix + strconv.Itoa(number)
}

func claimEligiblePullRequestInOrder(
	root string,
	repo providers.RepositoryRef,
	eligible []providers.PullRequestSummary,
) (*providers.PullRequestSummary, error) {
	runID, workflow, leaseDuration, err := pullRequestClaimParameters()
	if err != nil {
		return nil, err
	}
	return claimPullRequestInOrder(root, repo, eligible, runID, workflow, leaseDuration)
}

func pullRequestClaimParameters() (runID, workflow string, leaseDuration time.Duration, err error) {
	runID, workflow, err = providerRunContext()
	if err != nil {
		return "", "", 0, err
	}
	leaseDuration, err = pullRequestClaimLease()
	if err != nil {
		return "", "", 0, err
	}
	return runID, workflow, leaseDuration, nil
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
	ledger, err := openStageClaimLedger(l)
	if err != nil {
		return 0, false, fmt.Errorf("open claim ledger: %w", err)
	}
	var claimed string
	lockErr := ledger.Locked(claimContext(), claimLockOperationPRLookup, func(tx claimsclient.Ledger) error {
		entries, lerr := tx.ForRunAll(claimContext(), runID)
		if lerr != nil {
			return fmt.Errorf("read this run's claims: %w", lerr)
		}
		for _, entry := range entries {
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

// releasePullRequestClaimsForRun surrenders every PR lease runID holds. log
// may be nil when the stage is on the claims plane (the daemon's ledger
// journals the release there — claimLedgerJournal).
func releasePullRequestClaimsForRun(l instance.Layout, log *journal.InstanceLog, runID string) error {
	ledger, err := stageClaimLedgerForRun(l, l.Gaggle(), runID, withClaimJournal(log)...)
	if err != nil {
		return fmt.Errorf("open claim ledger: %w", err)
	}
	return ledger.Locked(claimContext(), claimLockOperationPRRelease, func(tx claimsclient.Ledger) error {
		entries, err := tx.ForRunAll(claimContext(), runID)
		if err != nil {
			return fmt.Errorf("read this run's claims: %w", err)
		}
		for _, entry := range entries {
			if !strings.HasPrefix(entry.ItemID, pullRequestClaimPrefix) {
				continue
			}
			if err := tx.ReleaseScoped(claimContext(), claimsclient.KeyForEntry(entry), runID); err != nil {
				return fmt.Errorf("release claim %s for run %s: %w", entry.ItemID, runID, err)
			}
		}
		return nil
	})
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

func claimPullRequestInOrder(
	root string,
	repo providers.RepositoryRef,
	candidates []providers.PullRequestSummary,
	runID, workflow string,
	leaseDuration time.Duration,
) (*providers.PullRequestSummary, error) {
	l := layoutFor(root)
	instanceLog, closeLog, err := claimLedgerJournal(l)
	if err != nil {
		return nil, err
	}
	defer closeLog()
	ledger, err := openStageClaimLedger(l, withClaimJournal(instanceLog)...)
	if err != nil {
		return nil, fmt.Errorf("open claim ledger: %w", err)
	}
	annotations, err := openStageAnnotator(l)
	if err != nil {
		return nil, fmt.Errorf("open instance annotations: %w", err)
	}
	defer func() { _ = annotations.Close() }()

	var selected *providers.PullRequestSummary
	err = ledger.Locked(claimContext(), claimLockOperationPRAcquire, func(tx claimsclient.Ledger) error {
		for _, candidate := range candidates {
			// The claim namespace must carry the repository's actual
			// provider (#3649): hardcoding github let an ADO or Gitea
			// repository's PR claim collide with a GitHub one of the same
			// number, so one provider's run could suppress another's.
			if gaggle := providerGaggle(); gaggle != "" && repo.Provider == "" {
				return fmt.Errorf(
					"claim PR #%d in ledger: repository provider identity is required for gaggle-scoped claims",
					candidate.Number,
				)
			}
			ok, _, err := tx.ClaimScoped(claimContext(), pullRequestClaimLedgerKey(providerGaggle(), repo.Provider, candidate.Number), runID, workflow, leaseDuration)
			if err != nil {
				return fmt.Errorf("claim PR #%d in ledger: %w", candidate.Number, err)
			}
			if ok {
				// #4417: record the PR's own typed repository identity
				// against this claim, durably, before returning it as
				// selected — terminal circuit-breaker/notification
				// bookkeeping later reads this back instead of
				// reconstructing ownership from the gaggle's static
				// project or cfg.Repos[0].
				itemID := pullRequestClaimKey(candidate.Number)
				if recordErr := recordItemRepository(annotations, runID, itemID, itemKindPullRequest, repo); recordErr != nil {
					return fmt.Errorf("claim PR #%d: %w", candidate.Number, recordErr)
				}
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

// pullRequestClaimLedgerKey addresses PR number's lease: scoped to the
// gaggle's repository-provider namespace, or legacy (unscoped) when the
// stage runs ungaggled — the split the ledger's Claim/ClaimScoped pair
// expressed. provider must be the claiming PR's own repository provider
// (#3649): a hardcoded provider let two repositories on different providers
// collide on the same PR number within one gaggle.
func pullRequestClaimLedgerKey(gaggle string, provider providers.ProviderKind, number int) claimsclient.Key {
	if gaggle == "" {
		return claimsclient.Key{ExternalID: pullRequestClaimKey(number)}
	}
	return claimsclient.Key{
		Gaggle:     gaggle,
		Provider:   string(provider),
		ExternalID: pullRequestClaimKey(number),
	}
}
