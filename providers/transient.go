package providers

import (
	"context"
	"errors"
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
	if errors.Is(err, context.DeadlineExceeded) {
		var urlErr *url.Error
		return errors.As(err, &urlErr) && urlErr.Timeout()
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	if m := statusCodePattern.FindStringSubmatch(err.Error()); m != nil {
		if code, convErr := strconv.Atoi(m[1]); convErr == nil {
			return code >= 500 || code == 429
		}
	}
	message := strings.ToLower(err.Error())
	for _, fragment := range transientMessageFragments {
		if strings.Contains(message, fragment) {
			return true
		}
	}
	return false
}

// statusCodePattern matches the "status %d" shape readJSONResponse's error
// messages use (see providers/http.go) — the only place a non-2xx GitHub
// response surfaces as a plain error string rather than a typed value.
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
