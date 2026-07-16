package providers

import (
	"context"
	"errors"
	"net"
	"regexp"
	"strconv"
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
// A context cancellation/deadline is never transient — it's the caller's own
// ctx ending, not the provider's request failing — so callers must let it
// propagate rather than retry it.
func IsTransientError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	// A typed rate-limit give-up (#614) is transient by definition: the quota
	// window resets on the clock, so a caller polling over minutes should
	// back off and try again, never treat it as permanent.
	var rl *RateLimitError
	if errors.As(err, &rl) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	if m := statusCodePattern.FindStringSubmatch(err.Error()); m != nil {
		if code, convErr := strconv.Atoi(m[1]); convErr == nil {
			return code >= 500 || code == 429
		}
	}
	return false
}

// statusCodePattern matches the "status %d" shape readJSONResponse's error
// messages use (see providers/http.go) — the only place a non-2xx GitHub
// response surfaces as a plain error string rather than a typed value.
var statusCodePattern = regexp.MustCompile(`status (\d{3})`)
