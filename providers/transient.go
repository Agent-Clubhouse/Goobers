package providers

import (
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// IsTransientError reports whether err looks like a transient/retryable
// provider failure — a network hiccup, a 5xx server error, or an exhausted
// rate-limit backoff — rather than a persistent one (auth, not-found,
// malformed request). GitHubProvider.send already retries these within a
// single request up to its own maxRetries budget; this classifier is for a
// caller polling in a loop over many minutes (internal/executor's
// CIPollExecutor, #239) that needs to tell "back off and try again" apart
// from "stop, this will never succeed" once even that per-request budget is
// exhausted.
//
// A caller context cancellation/deadline is never transient. A URL transport
// timeout that wraps context.DeadlineExceeded is transient, because it is the
// provider request rather than the caller ending.
func IsTransientError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	// A typed rate-limit give-up (#614) is transient by definition: the quota
	// window resets on the clock, so a caller polling over minutes should
	// back off and try again, never treat it as permanent.
	var rl *RateLimitError
	if errors.As(err, &rl) {
		return true
	}
	var urlErr *url.Error
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.As(err, &urlErr) && urlErr.Timeout()
	}
	if errors.As(err, &urlErr) && errors.Is(urlErr.Err, io.EOF) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	message := strings.ToLower(err.Error())
	var responseErr *providerResponseError
	if errors.As(err, &responseErr) {
		return isTransientStatus(responseErr.statusCode, responseErr.hasRetryGuidance())
	}
	if m := statusCodePattern.FindStringSubmatch(message); m != nil {
		if code, convErr := strconv.Atoi(m[1]); convErr == nil {
			return isTransientStatus(code, hasRateLimitRetryGuidance(message))
		}
	}
	if strings.Contains(message, "send request:") && strings.HasSuffix(strings.TrimSpace(message), ": eof") {
		return true
	}
	for _, fragment := range transientMessageFragments {
		if strings.Contains(message, fragment) {
			return true
		}
	}
	return false
}

func isTransientStatus(code int, guidedRateLimit bool) bool {
	return code >= 500 || code == 429 || code == 403 && guidedRateLimit
}

func hasRateLimitRetryGuidance(message string) bool {
	for _, fragment := range []string{
		"retry after ",
		"retry-after:",
		"retry-after=",
		"x-ratelimit-remaining: 0",
		"x-ratelimit-remaining=0",
		`x-ratelimit-remaining="0"`,
		"x-ratelimit-reset",
	} {
		if strings.Contains(message, fragment) {
			return true
		}
	}
	return false
}

// statusCodePattern matches provider errors that cross a subprocess boundary
// and therefore no longer retain providerResponseError's typed metadata.
var statusCodePattern = regexp.MustCompile(`status (\d{3})`)

// transientMessageFragments covers the transport errors that cross the
// built-in stage's subprocess boundary as stderr text and therefore no longer
// retain their net.Error type.
var transientMessageFragments = []string{
	"client.timeout exceeded",
	"connection aborted",
	"connection refused",
	"connection reset",
	"connection timed out",
	"i/o timeout",
	"network is unreachable",
	"no such host",
	"server misbehaving",
	"temporary failure",
	"tls handshake timeout",
	"unexpected eof",
}
