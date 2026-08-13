package providers

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// githubSelfReviewMarker is the stable fragment GitHub returns in its
// categorical refusal to let an account review its own pull request —
// present in both the APPROVE message ("Can not approve your own pull
// request") and the REQUEST_CHANGES message ("Can not request changes on
// your own pull request"). Matching the shared tail rather than either full
// message covers both review events with one predicate.
const githubSelfReviewMarker = "your own pull request"

// giteaSelfReviewMarker is Gitea's shorter categorical refusal ("approve your
// own pull is not allowed") — the same refusal as githubSelfReviewMarker, one
// word shorter, so it needs its own exact fragment rather than reusing
// GitHub's.
//
// A single combined marker ("your own pull") once covered both forges with
// one predicate, but that fragment is also a substring of unrelated 422
// bodies that merely mention "your own pull" in passing (e.g. a differently
// worded validation or branch-protection message), which would misclassify
// them as a self-review refusal. Keeping two explicit, provider-specific
// markers stays precise on both forges without that false-positive surface.
const giteaSelfReviewMarker = "your own pull is not allowed"

// isSelfReviewBody reports whether a 422 response body is either forge's
// categorical self-review refusal.
func isSelfReviewBody(body string) bool {
	lower := strings.ToLower(body)
	return strings.Contains(lower, githubSelfReviewMarker) || strings.Contains(lower, giteaSelfReviewMarker)
}

// IsSelfReviewError reports whether err is a forge's categorical refusal to let
// an account submit a native Review on its own pull request — an HTTP 422 whose
// body carries GitHub's or Gitea's self-review message (see
// githubSelfReviewMarker/giteaSelfReviewMarker). Neither GitHub nor Gitea makes
// this configurable, and it never succeeds on retry.
//
// It fires whenever the reviewing identity is also the PR author. On an
// instance with a single credential backing both github:pr:write (opens the
// PR) and github:pr:review (reviews it), that is EVERY daemon-authored PR
// (#870). A caller that can fall back to a non-native handoff — publishing the
// verdict as a label/comment — should treat this as a soft skip rather than a
// hard failure, since a self-authored native Review carries no value the
// platform would honor anyway (neither forge counts a self-approval toward a
// required-review rule).
func IsSelfReviewError(err error) bool {
	if err == nil {
		return false
	}
	var responseErr *providerResponseError
	if errors.As(err, &responseErr) {
		return responseErr.statusCode == http.StatusUnprocessableEntity &&
			isSelfReviewBody(responseErr.body)
	}
	// Subprocess-crossed or already-stringified error (the typed value did not
	// survive): match the same 422 + marker in the flattened message, mirroring
	// IsTransientError's string-fallback discipline.
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "status 422") && isSelfReviewBody(msg)
}

// IsFineGrainedPATReviewNotFoundError reports whether err has the opaque shape
// GitHub returns when a fine-grained PAT attempts to review its own pull
// request. The response alone is ambiguous, so callers must also confirm that
// the reviewing identity authored the pull request before degrading.
func IsFineGrainedPATReviewNotFoundError(err error) bool {
	var responseErr *providerResponseError
	if !errors.As(err, &responseErr) ||
		responseErr.statusCode != http.StatusNotFound ||
		responseErr.method != http.MethodPost {
		return false
	}
	endpoint, parseErr := url.Parse(responseErr.endpoint)
	if parseErr != nil {
		return false
	}
	parts := strings.Split(strings.Trim(endpoint.Path, "/"), "/")
	if len(parts) < 3 ||
		parts[len(parts)-3] != "pulls" ||
		parts[len(parts)-2] == "" ||
		parts[len(parts)-1] != "reviews" {
		return false
	}
	var body struct {
		Message string `json:"message"`
	}
	return json.Unmarshal([]byte(responseErr.body), &body) == nil &&
		body.Message == http.StatusText(http.StatusNotFound)
}
