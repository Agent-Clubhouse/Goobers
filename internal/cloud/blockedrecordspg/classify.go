package blockedrecordspg

import (
	"context"
	"errors"
	"regexp"

	"github.com/goobers/goobers/internal/blockedcycle"
	"github.com/goobers/goobers/internal/terminalhooks/providerwriter"
	"github.com/goobers/goobers/providers"
)

// unverified is a cycle member whose open state could not be confirmed.
type unverified struct {
	Node blockedcycle.Node
	// Class names why, for the escalation comment ("rate limited", "not found").
	Class string
	// Permanent failures can never succeed on retry (the repository is not
	// configured, the credential is refused, the item is gone) and are not retried;
	// retryable ones are re-read inline a bounded number of times.
	Permanent bool
}

// VerifyError is a verification failure whose text has already been scrubbed but
// whose kind is kept, so the store can still tell a retryable failure from a
// permanent one after the provider's own error (with whatever it echoed) is gone.
type VerifyError struct {
	Class     string
	Permanent bool
	Message   string
}

func (e *VerifyError) Error() string { return e.Message }

// ClassifyVerification sorts a verification error into a class and whether
// retrying can help; callers that scrub an error build a VerifyError from it
// first.
func ClassifyVerification(err error) (class string, permanent bool) { return classify(err) }

// notFoundMessage recognises a 404 whose typed error was lost crossing a boundary
// (the writer reports provider errors as text).
var notFoundMessage = regexp.MustCompile(`\bstatus 404\b`)

// classify sorts a verification error into a class and whether retrying can help.
// Retryable: a rate limit, a timeout, a 5xx or a network failure, and anything it
// cannot recognise (held, but bounded by the hold). Permanent: the repository is
// not pinned or the writer refused permanently, or the provider answered
// 401, 403 or 404.
func classify(err error) (class string, permanent bool) {
	var kept *VerifyError
	if errors.As(err, &kept) {
		return kept.Class, kept.Permanent
	}
	switch {
	case errors.Is(err, providerwriter.ErrRepositoryNotPinned):
		return "repository not configured", true
	case errors.Is(err, providerwriter.ErrPermanent):
		return "refused", true
	}
	if _, limited := providers.AsRateLimitError(err); limited {
		return "rate limited", false
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out", false
	case providers.IsTransientError(err):
		return "provider unavailable", false
	case providers.IsNotFoundError(err), notFoundMessage.MatchString(err.Error()):
		return "not found", true
	case providers.IsAuthenticationError(err):
		return "unauthorized", true
	}
	return "error", false
}
