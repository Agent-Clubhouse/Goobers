package childengine

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/cloud/acceptance"
	cloudlease "github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/objstore"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// Refusals the adapter itself decides; retrying the same request cannot help.
var (
	errNotLiveChild   = errors.New("childengine: handoff does not name a live accepted child")
	errBindingDiffers = errors.New("childengine: binding differs from the recorded start intent")
)

// classify wraps a custody failure in the engine's error class so the plane
// answers 503 (retry through a failover) or a 4xx (never retry). Anything it
// does not recognize stays unclassified: a bounded engine retry, then stop.
func classify(err error) error {
	switch {
	case err == nil, errors.Is(err, engine.ErrChildCustodyRefused), errors.Is(err, engine.ErrChildCustodyUnavailable):
		return err
	case errors.Is(err, cloudlease.ErrStale), errors.Is(err, cloudlease.ErrSelfFenced),
		errors.Is(err, cloudlease.ErrFencedTransactionTimeout), errors.Is(err, cloudlease.ErrHeld),
		errors.Is(err, acceptance.ErrStaleChildEpoch), errors.Is(err, objstore.ErrGenerationUnavailable),
		pgconn.SafeToRetry(err):
		return fmt.Errorf("%w: %w", engine.ErrChildCustodyUnavailable, err)
	case errors.Is(err, errNotLiveChild), errors.Is(err, errBindingDiffers), errors.Is(err, errResultDiffers), errors.Is(err, sql.ErrNoRows),
		errors.Is(err, triggerqueue.ErrParentCancelled), errors.Is(err, triggerqueue.ErrParentSettled),
		errors.Is(err, triggerqueue.ErrChildAuthorityChanged), errors.Is(err, triggerqueue.ErrChildProposalUnavailable),
		// A sealed result never changes, and one that does not verify is not
		// repaired by retrying.
		errors.Is(err, triggerqueue.ErrConflict), errors.Is(err, triggerqueue.ErrChildResultUnavailable),
		// The accepted child no longer reproduces from its pinned generation.
		errors.Is(err, childworkflow.ErrFrozenChildDrift), errors.Is(err, childworkflow.ErrAuthorityUnavailable), errors.Is(err, childworkflow.ErrSubmissionInvalid),
		errors.Is(err, objstore.ErrGenerationNotFound), errors.Is(err, objstore.ErrGenerationInvalid):
		return fmt.Errorf("%w: %w", engine.ErrChildCustodyRefused, err)
	default:
		return err
	}
}
