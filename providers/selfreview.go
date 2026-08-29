package providers

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// GitHub and Gitea use different categorical self-review refusals. Keep their
// stable fragments separate rather than broadening the match to "your own
// pull", which can occur in unrelated validation errors.
const (
	githubSelfReviewMarker = "your own pull request"
	giteaSelfReviewMarker  = "your own pull is not allowed"
)

func isSelfReviewBody(body string) bool {
	lower := strings.ToLower(body)
	return strings.Contains(lower, githubSelfReviewMarker) ||
		strings.Contains(lower, giteaSelfReviewMarker)
}

// IsSelfReviewError reports whether err is a forge's categorical refusal to let
// an account submit a native Review on its own pull request — an HTTP 422 whose
// body carries one of the provider-specific self-review messages. Neither
// GitHub nor Gitea makes this configurable, and it never succeeds on retry.
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
