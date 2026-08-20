package executor

import (
	"errors"
	"fmt"
	"testing"

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

// TestTransientPollCode proves a retryable ci-poll failure names its own
// cause: a rate-limited poll and a flaky 5xx are the same retry decision but
// different operator problems.
func TestTransientPollCode(t *testing.T) {
	rateLimited := fmt.Errorf("poll checks: %w", &providers.RateLimitError{})
	if got := transientPollCode(rateLimited); got != providers.ErrorCodeRateLimited {
		t.Fatalf("transientPollCode(rate limit) = %q, want %q", got, providers.ErrorCodeRateLimited)
	}
	if got := transientPollCode(errors.New("status 503: unavailable")); got != "poll_provider_error" {
		t.Fatalf("transientPollCode(5xx) = %q, want poll_provider_error", got)
	}
}
