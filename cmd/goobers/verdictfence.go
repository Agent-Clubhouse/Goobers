package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"github.com/goobers/goobers/internal/claimrelease"
	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

var standalonePublicationID atomic.Uint64

type publicationCustodyKey struct{}
type publicationCustodyState struct{ retain bool }

// withVerdictPublicationCustody serializes authoritative publication with the
// final ledger read through Land. Unlike the older long merge window, BOTH
// local stages and plane stages acquire the same durable claim, so mixed
// deployments cannot enter different lock domains. The claim is run-owned:
// participating stages in one run must execute sequentially.
func withVerdictPublicationCustody(ctx context.Context, root string, repo providers.RepositoryRef, fn func(context.Context) error) error {
	ledger, err := openStageClaimLedger(layoutFor(root))
	if err != nil {
		return fmt.Errorf("open verdict publication custody: %w", err)
	}
	key := claimsclient.MergeLockKey(providerGaggle(), string(repo.Provider), repo.Owner, repo.Name)
	key.ExternalID = localscheduler.PublicationLockItemIDPrefix + string(repo.Provider) + "/" + repo.Owner + "/" + repo.Name
	// Older local invocations have no gaggle. Preserve their unscoped ledger
	// namespace; provider/repository identity remains in the synthetic item.
	if key.Gaggle == "" && !claimsPlaneSelected() {
		key.Provider = ""
	}
	runID := os.Getenv(executor.RunIDEnvVar)
	if runID == "" && !claimsPlaneSelected() {
		runID = fmt.Sprintf("publication-%d-%d", os.Getpid(), standalonePublicationID.Add(1))
	}
	lock := claimsclient.MergeLock{Key: key, RunID: runID, Workflow: os.Getenv(executor.WorkflowEnvVar)}
	return withPublicationClaim(ctx, ledger, lock, 2*time.Minute, time.Minute, 5*time.Minute, fn)
}

// The bounded action ends well before the durable lease. No renewal can
// silently reacquire an expired claim. If a callback ignores cancellation or
// the provider's completion is uncertain, retain the lease until expiry and
// report that uncertainty; never report a successful guarded landing.
func withPublicationClaim(ctx context.Context, ledger claimsclient.Ledger, lock claimsclient.MergeLock, wait, action, lease time.Duration, fn func(context.Context) error) error {
	if lock.RunID == "" || action <= 0 || lease <= action {
		return errors.New("invalid verdict publication custody")
	}
	acquireCtx, stopAcquire := context.WithTimeout(ctx, wait)
	defer stopAcquire()
	var started time.Time
	for {
		if err := acquireCtx.Err(); err != nil {
			return fmt.Errorf("wait for verdict publication custody: %w", err)
		}
		started = time.Now()
		ok, err := claimPublicationIfUnheld(acquireCtx, ledger, lock, lease)
		if err != nil {
			return fmt.Errorf("acquire verdict publication custody: %w", err)
		}
		if ok {
			break
		}
		select {
		case <-acquireCtx.Done():
			return fmt.Errorf("wait for verdict publication custody: %w", acquireCtx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
	// Include acquisition latency in the action bound, even when the local
	// backend had to wait for a file lock and could not honor cancellation.
	actionCtx, cancel := context.WithDeadline(ctx, started.Add(action))
	defer cancel()
	custody := &publicationCustodyState{}
	actionCtx = context.WithValue(actionCtx, publicationCustodyKey{}, custody)
	var err error
	if actionCtx.Err() == nil {
		err = fn(actionCtx)
	}
	actionErr := actionCtx.Err()
	verifyCtx, stopVerify := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer stopVerify()
	entries, verifyErr := ledger.ForRunAll(verifyCtx, lock.RunID)
	held := false
	for _, entry := range entries {
		if claimsclient.KeyForEntry(entry) == lock.Key && entry.ExpiresAt.After(time.Now()) {
			held = true
		}
	}
	if actionErr != nil {
		return errors.Join(err, verifyErr, fmt.Errorf("verdict publication custody deadline exceeded; publication/landing outcome uncertain; claim retained until expiry (held=%t): %w", held, actionErr))
	}
	if verifyErr != nil || !held || !time.Now().Before(started.Add(lease)) {
		return errors.Join(err, fmt.Errorf("verdict publication custody lost or unverifiable after action; publication/landing outcome uncertain (held=%t): %w", held, errors.Join(verifyErr, errors.New("custody verification failed"))))
	}
	if custody.retain {
		return err // Preserve the caller's original landing-result/error reporting.
	}
	releaseErr := ledger.ReleaseScoped(verifyCtx, lock.Key, lock.RunID, claimrelease.Completed.Because(localscheduler.PublicationLockReleaseCode, ""))
	return errors.Join(err, releaseErr)
}

// ClaimScoped normally renews an existing same-run claim. After an uncertain
// completion that would defeat quarantine: a retry must wait for its retained
// claim to expire, just like another run. Sequential stages per run are required
// because separate same-run callers cannot atomically distinguish ownership.
func claimPublicationIfUnheld(ctx context.Context, ledger claimsclient.Ledger, lock claimsclient.MergeLock, lease time.Duration) (bool, error) {
	entries, err := ledger.ForRunAll(ctx, lock.RunID)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if claimsclient.KeyForEntry(entry) == lock.Key && entry.ExpiresAt.After(time.Now()) {
			return false, nil
		}
	}
	ok, _, err := ledger.ClaimScoped(ctx, lock.Key, lock.RunID, lock.Workflow, lease)
	return ok, err
}

// Publication may commit during a long pre-merge check or policy lookup.
// Re-read its authoritative ledger under custody immediately before Land.
func withFindingCheckedLanding(ctx context.Context, root string, provider any, repo providers.RepositoryRef, pull, author string, poll providers.PullRequestPollResult, reasons *[]string, land func(context.Context) error) error {
	return withVerdictPublicationCustody(ctx, root, repo, func(ctx context.Context) error {
		*reasons = append(*reasons, outstandingFindingMergeRefusals(ctx, root, provider, repo, pull, author, poll)...)
		if len(*reasons) > 0 {
			return nil
		}
		return land(ctx)
	})
}
