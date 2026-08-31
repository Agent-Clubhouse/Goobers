package executor

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/providers"
)

// stageCoded mirrors the one-method interface internal/runner matches with
// errors.As. Declaring it here (rather than importing the runner) is the
// point: the seam must work for any consumer without a package dependency.
type stageCoded interface {
	error
	StageErrorCode() string
}

// TestProviderStageInfrastructureFailureCarriesTypedCode is the seam
// acceptance for the audit's biggest bucket: a RETRYABLE provider failure is
// wrapped for infrastructure retry, and before this its typed code
// (github_auth_failed, github_rate_limited, claims_lock_timeout) existed only
// inside the message, so every such attempt journaled as executor_error.
func TestProviderStageInfrastructureFailureCarriesTypedCode(t *testing.T) {
	for _, code := range []string{
		providers.ErrorCodeAuthFailed,
		providers.ErrorCodeRateLimited,
		"claims_lock_timeout",
	} {
		t.Run(code, func(t *testing.T) {
			err := providerStageInfrastructureFailure("open-pr", code, "provider said no", nil)
			if !invoke.IsInfrastructureFailure(err) {
				t.Fatalf("err = %v, want it to keep the infrastructure-retry marker", err)
			}
			var coded stageCoded
			if !errors.As(err, &coded) {
				t.Fatalf("err = %v (%T), want a code-carrying failure", err, err)
			}
			if got := coded.StageErrorCode(); got != code {
				t.Fatalf("StageErrorCode() = %q, want %q", got, code)
			}
			want := fmt.Sprintf("executor: provider stage %q reported %s: provider said no", "open-pr", code)
			if err.Error() != want {
				t.Fatalf("message = %q, want it preserved verbatim (%q)", err.Error(), want)
			}
		})
	}
}

// TestStageFailureIsTransparent proves the wrapper adds a code without
// changing anything a caller already relies on: nil stays nil, an
// un-coded failure is returned untouched, and errors.Is still reaches the
// wrapped cause.
func TestStageFailureIsTransparent(t *testing.T) {
	if err := StageFailure("infra_git_failed", nil); err != nil {
		t.Fatalf("StageFailure(nil) = %v, want nil", err)
	}
	cause := errors.New("boom")
	// Identity, not just equivalence: an empty code must return the very
	// error it was handed, never a wrapper that changes what %T reports.
	if err := StageFailure("", cause); !errors.Is(err, cause) || err.Error() != cause.Error() {
		t.Fatalf("StageFailure with an empty code = %v, want the error unchanged", err)
	}
	var coded stageCoded
	if errors.As(StageFailure("", cause), &coded) {
		t.Fatal("an empty code produced a code-carrying failure")
	}
	wrapped := StageFailure("timeout", fmt.Errorf("executor: stage %q: %w", "local-ci", cause))
	if !errors.Is(wrapped, cause) {
		t.Fatalf("errors.Is(%v, cause) = false, want the cause still reachable", wrapped)
	}
}

// TestCIPollFailureCode proves a ci-poll failure names its own cause: a
// rate-limited poll and a flaky 5xx are the same retry decision but different
// operator problems. Exported (#3881) because the POD path names the failure
// with this same function; a second spelling on that side would journal one
// stage under two codes depending on where it ran.
func TestCIPollFailureCode(t *testing.T) {
	rateLimited := fmt.Errorf("poll checks: %w", &providers.RateLimitError{})
	if got := CIPollFailureCode(rateLimited); got != providers.ErrorCodeRateLimited {
		t.Fatalf("CIPollFailureCode(rate limit) = %q, want %q", got, providers.ErrorCodeRateLimited)
	}
	if got := CIPollFailureCode(errors.New("status 503: unavailable")); got != CIPollProviderErrorCode {
		t.Fatalf("CIPollFailureCode(5xx) = %q, want %q", got, CIPollProviderErrorCode)
	}
}

// TestCIPollRateLimitReset pins the OTHER half of what a pod ci-poll must
// report (decision 005 C5): the reset instant, RFC3339, so the daemon's live
// RateLimited observer can record the exhausted window against
// ProviderQuotaState. A rate limit that carried no reset header reports
// nothing rather than the zero time — the consumers skip a missing reset, and
// would park the scheduler at the epoch on a zero one.
func TestCIPollRateLimitReset(t *testing.T) {
	reset := time.Date(2026, 8, 29, 18, 30, 0, 0, time.UTC)
	err := fmt.Errorf("poll checks: %w", &providers.RateLimitError{Reset: reset})
	got, ok := CIPollRateLimitReset(err)
	if !ok || got != "2026-08-29T18:30:00Z" {
		t.Fatalf("CIPollRateLimitReset(rate limit) = %q, %t; want 2026-08-29T18:30:00Z, true", got, ok)
	}
	if _, ok := CIPollRateLimitReset(fmt.Errorf("wrapped: %w", &providers.RateLimitError{})); ok {
		t.Fatal("a rate limit with no reset header reported one")
	}
	if _, ok := CIPollRateLimitReset(errors.New("status 503: unavailable")); ok {
		t.Fatal("a non-rate-limit error reported a reset")
	}
}
