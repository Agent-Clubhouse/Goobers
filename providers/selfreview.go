package providers

import (
	"errors"
	"net/http"
	"strings"
)

// selfReviewMarker is the stable fragment GitHub returns in its categorical
// refusal to let an account review its own pull request — present in both the
// APPROVE message ("Can not approve your own pull request") and the
// REQUEST_CHANGES message ("Can not request changes on your own pull
// request"). Matching the shared tail rather than either full message covers
// both review events with one predicate.
const selfReviewMarker = "your own pull request"

// IsSelfReviewError reports whether err is GitHub's categorical refusal to let
// an account submit a native Review on its own pull request — an HTTP 422 whose
// body carries the "…your own pull request" message. GitHub enforces this
// regardless of token scope; it is not configurable and never succeeds on
// retry.
//
// It fires whenever the reviewing identity is also the PR author. On an
// instance with a single GitHub credential backing both github:pr:write (opens
// the PR) and github:pr:review (reviews it), that is EVERY daemon-authored PR
// (#870). A caller that can fall back to a non-native handoff — publishing the
// verdict as a label/comment — should treat this as a soft skip rather than a
// hard failure, since a self-authored native Review carries no value the
// platform would honor anyway (GitHub never counts a self-approval toward a
// required-review rule).
func IsSelfReviewError(err error) bool {
	if err == nil {
		return false
	}
	var responseErr *providerResponseError
	if errors.As(err, &responseErr) {
		return responseErr.statusCode == http.StatusUnprocessableEntity &&
			strings.Contains(strings.ToLower(responseErr.body), selfReviewMarker)
	}
	// Subprocess-crossed or already-stringified error (the typed value did not
	// survive): match the same 422 + marker in the flattened message, mirroring
	// IsTransientError's string-fallback discipline.
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "status 422") && strings.Contains(msg, selfReviewMarker)
}
