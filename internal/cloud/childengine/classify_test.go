package childengine

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/goobers/goobers/internal/cloud/acceptance"
	cloudlease "github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/cloud/objstore"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func TestClassifySeparatesFailoverFromPermanentRefusal(t *testing.T) {
	for _, err := range []error{cloudlease.ErrStale, cloudlease.ErrSelfFenced, cloudlease.ErrFencedTransactionTimeout, cloudlease.ErrHeld, fmt.Errorf("fenced write: %w", cloudlease.ErrStale), acceptance.ErrStaleChildEpoch, objstore.ErrGenerationUnavailable} {
		if got := classify(err); !errors.Is(got, engine.ErrChildCustodyUnavailable) || !errors.Is(got, err) || errors.Is(got, engine.ErrChildCustodyRefused) {
			t.Errorf("classify(%v) = %v, want unavailable", err, got)
		}
	}
	for _, err := range []error{errNotLiveChild, errBindingDiffers, sql.ErrNoRows, triggerqueue.ErrParentCancelled, triggerqueue.ErrParentSettled, triggerqueue.ErrChildAuthorityChanged, triggerqueue.ErrChildProposalUnavailable} {
		if got := classify(err); !errors.Is(got, engine.ErrChildCustodyRefused) || !errors.Is(got, err) || errors.Is(got, engine.ErrChildCustodyUnavailable) {
			t.Errorf("classify(%v) = %v, want refused", err, got)
		}
	}
	plain := errors.New("unexpected")
	if got := classify(plain); got != plain { //nolint:errorlint // Identity: an unclassified error must pass through unwrapped.
		t.Errorf("unclassified error rewritten to %v", got)
	}
	already := fmt.Errorf("x: %w", engine.ErrChildCustodyRefused)
	if got := classify(already); got != already || classify(nil) != nil { //nolint:errorlint // Identity: an unclassified error must pass through unwrapped.
		t.Errorf("classified or nil errors were rewrapped: %v", got)
	}
}
