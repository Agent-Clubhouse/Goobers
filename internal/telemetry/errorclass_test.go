package telemetry

import "testing"

// TestClassifyErrorNamesEveryProducedCode is the taxonomy's contract with its
// producers: every code the runner, the executor, and the provider-chain
// subcommands actually emit must land in a class that names an OWNER. Each
// code below classified as "unknown" before this table existed, which is how
// one gaggle reported 100% of its failures — including provider-classified
// ones — as unclassified.
func TestClassifyErrorNamesEveryProducedCode(t *testing.T) {
	tests := []struct {
		code string
		want ErrorClass
	}{
		{code: "", want: ""},
		{code: ErrCodeExecutor, want: ErrorClassExecutor},
		{code: ErrCodeInfraGit, want: ErrorClassInfraGit},
		{code: ErrCodeInfraNet, want: ErrorClassInfraNet},
		{code: ErrCodeInfraWorkspace, want: ErrorClassInfra},
		{code: ErrCodeClaimsLock, want: ErrorClassInfraLock},
		{code: ErrCodeCredentialUnavailable, want: ErrorClassInfra},
		{code: ErrCodeGitHubAuth, want: ErrorClassProvider},
		{code: ErrCodeProviderFailed, want: ErrorClassProvider},
		{code: ErrCodePollProvider, want: ErrorClassProvider},
		{code: "github_rate_limited", want: ErrorClassProviderRateLimit},
		// Unlisted codes still reach their owner through the namespace
		// heuristics rather than falling back to unknown.
		{code: "github_pr_not_found", want: ErrorClassProvider},
		{code: "infra_git_worktree_locked", want: ErrorClassInfraGit},
		{code: "infra_net_proxy_refused", want: ErrorClassInfraNet},
		{code: "provider_chain_unavailable", want: ErrorClassProvider},
		// Pre-existing classifications must not regress.
		{code: ErrCodeProviderRateLimit, want: ErrorClassProviderRateLimit},
		{code: ErrCodeTimeout, want: ErrorClassTimeout},
		{code: "timeout_waiting_for_ci", want: ErrorClassTimeout},
		{code: ErrCodeHarnessFailure, want: ErrorClassHarnessFailure},
		{code: "harness.crashed", want: ErrorClassHarnessFailure},
		{code: ErrCodeValidationFailed, want: ErrorClassValidation},
		{code: "schema.invalid", want: ErrorClassValidation},
		{code: ErrCodeInfraFailure, want: ErrorClassInfra},
		// A code nobody has taught the taxonomy stays deliberately unknown,
		// so "not yet seen" remains distinguishable from "classified".
		{code: "blocked_by_agent", want: ErrorClassUnknown},
		{code: "claim_recovery_failed", want: ErrorClassUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.code, func(t *testing.T) {
			if got := ClassifyError(tt.code); got != tt.want {
				t.Fatalf("ClassifyError(%q) = %q, want %q", tt.code, got, tt.want)
			}
		})
	}
}

// TestClaimsLockTimeoutIsContentionNotDuration pins the one reclassification
// this change makes: claims_lock_timeout used to fall into the generic
// "timeout" class through a substring match, which put lock contention (fix:
// reduce concurrency) in the same bucket as a stage running long (fix: raise
// timeoutSeconds).
func TestClaimsLockTimeoutIsContentionNotDuration(t *testing.T) {
	if got := ClassifyError(ErrCodeClaimsLock); got != ErrorClassInfraLock {
		t.Fatalf("ClassifyError(%q) = %q, want %q", ErrCodeClaimsLock, got, ErrorClassInfraLock)
	}
	if got := ClassifyError("stage_timeout"); got != ErrorClassTimeout {
		t.Fatalf("ClassifyError(stage_timeout) = %q, want %q", got, ErrorClassTimeout)
	}
}
