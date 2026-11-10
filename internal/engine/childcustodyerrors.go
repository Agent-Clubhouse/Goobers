package engine

import (
	"context"
	"errors"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
)

// Child custody error classes. A ChildService wraps its failures with one of
// these so the engine retries only what a retry can fix.
var (
	// ErrChildCustodyRefused is a permanent refusal: the same request can never
	// succeed, so the activity fails without retrying.
	ErrChildCustodyRefused = errors.New("engine: child custody refused the request")
	// ErrChildCustodyUnavailable is a transient loss of the custody owner, such
	// as a control-plane failover; the activity retries for its whole
	// schedule-to-close budget.
	ErrChildCustodyUnavailable = errors.New("engine: child custody is temporarily unavailable")
)

const (
	// childUnclassifiedAttempts preserves the original bounded retry for
	// failures a ChildService does not classify.
	childUnclassifiedAttempts = 3
	childCustodyRefusedType   = "child_custody_refused"
	childCustodyFailedType    = "child_custody_failed"
)

// classifyChildCustody maps a child custody activity failure onto Temporal
// retry semantics for the current attempt.
func classifyChildCustody(ctx context.Context, err error) error {
	attempt := int32(1)
	if activity.IsActivity(ctx) {
		attempt = activity.GetInfo(ctx).Attempt
	}
	return classifyChildCustodyAttempt(err, attempt)
}

// classifyChildCustodyAttempt keeps unavailable failures retryable and makes
// refusals, and unclassified failures past their attempt budget, final.
func classifyChildCustodyAttempt(err error, attempt int32) error {
	var applicationErr *temporal.ApplicationError
	switch {
	case err == nil, errors.Is(err, ErrChildCustodyUnavailable):
		return err
	case errors.As(err, &applicationErr) && applicationErr.NonRetryable():
		return err
	case errors.Is(err, ErrChildCustodyRefused):
		return temporal.NewNonRetryableApplicationError(err.Error(), childCustodyRefusedType, err)
	case attempt >= childUnclassifiedAttempts:
		return temporal.NewNonRetryableApplicationError(err.Error(), childCustodyFailedType, err)
	default:
		return err
	}
}
