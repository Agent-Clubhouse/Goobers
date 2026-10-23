package scheduling

// Ledger and lock filenames are shared by the scheduler services and CLI
// commands so every caller uses the same durable state and lock domains.
const (
	ClaimLedgerFileName = "claims.json"
	ClaimLockFileName   = "claims.lock"
	// MergeLockFileName is the cross-process file lock `merge-pr` (mergepr.go)
	// takes around its poll->decide->merge window (issue #719): once
	// merge-review's readiness allows several concurrent runs to review
	// DIFFERENT PRs at once, each independently reaches merge-pr, but only
	// one PR may be inside that window at a time — serializing it is what
	// lets a later run's re-poll (D6) actually observe an earlier run's
	// just-landed merge instead of racing it. A dedicated lock file, not
	// ClaimLockFileName, since this guards a different critical section
	// (the merge decision, not claim-ledger mutation) with a different
	// hold-time profile (network round-trips, not a fast local read/write).
	MergeLockFileName          = "merge.lock"
	PostMergeReconcileLockFile = "post-merge-reconcile.lock"
	SiblingCacheLockFileName   = "sibling-context-cache.lock"
)

// Claim lock operation labels are shared by lifecycle services and CLI commands.
const (
	ClaimLockOperationRecovery   = "claim-recovery"
	ClaimLockOperationRunRelease = "run-claims.release"
)
