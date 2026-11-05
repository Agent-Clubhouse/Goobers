package engine

import (
	"errors"
	"fmt"
	"testing"

	"go.temporal.io/sdk/temporal"
)

func nonRetryable(err error) bool {
	var applicationErr *temporal.ApplicationError
	return errors.As(err, &applicationErr) && applicationErr.NonRetryable()
}

func TestChildCustodyErrorsRetryOnlyWhatARetryCanFix(t *testing.T) {
	plain := errors.New("custody hiccup")
	unavailable := fmt.Errorf("control failover: %w", ErrChildCustodyUnavailable)
	refused := fmt.Errorf("lineage closed: %w", ErrChildCustodyRefused)
	final := temporal.NewNonRetryableApplicationError("bad manifest", "child_manifest_invalid", nil)
	for _, tc := range []struct {
		name     string
		err      error
		attempt  int32
		wantStop bool
	}{
		{name: "nil", attempt: 1},
		{name: "unavailable first attempt", err: unavailable, attempt: 1},
		{name: "unavailable keeps retrying past the unclassified budget", err: unavailable, attempt: 40},
		{name: "refused stops immediately", err: refused, attempt: 1, wantStop: true},
		{name: "unclassified retries within budget", err: plain, attempt: childUnclassifiedAttempts - 1},
		{name: "unclassified stops at budget", err: plain, attempt: childUnclassifiedAttempts, wantStop: true},
		{name: "already final stays final", err: final, attempt: 1, wantStop: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyChildCustodyAttempt(tc.err, tc.attempt)
			if tc.err == nil {
				if got != nil {
					t.Fatalf("nil classified as %v", got)
				}
				return
			}
			if !errors.Is(got, tc.err) && !errors.Is(got, final) {
				t.Fatalf("classification lost the cause: %v", got)
			}
			if nonRetryable(got) != tc.wantStop {
				t.Fatalf("non-retryable = %v, want %v (%v)", nonRetryable(got), tc.wantStop, got)
			}
		})
	}
}

func TestChildCustodyRetryBudgetOutlastsAControlFailover(t *testing.T) {
	// Lease TTL (15s) plus self-fence (10s) plus readiness must fit many
	// capped retries inside the schedule-to-close budget.
	if childCustodyRetryCeiling*6 > childCleanupTimeout/2 {
		t.Fatalf("retry ceiling %s leaves too few probes in %s", childCustodyRetryCeiling, childCleanupTimeout)
	}
}
