package providers

import (
	"errors"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	defaultRateLimitRetries = 4
	rateLimitBackoffBase    = time.Second
	rateLimitBackoffMax     = 60 * time.Second
	// Server clocks and reset windows can be slightly ahead of the client.
	rateLimitResetSlack     = 2 * time.Second
	defaultRateLimitMaxWait = 5 * time.Minute
	// maxDurationSeconds is the largest whole number of seconds a
	// time.Duration can hold; server-directed delays beyond it are rejected
	// rather than allowed to overflow.
	maxDurationSeconds = (1<<63 - 1) / int64(time.Second)
)

// AsRateLimitError recovers a typed rate-limit error from err, provider-
// neutrally (#3647). A give-up *RateLimitError — the shape GitHub's send()
// returns once its in-request backoff budget is spent — is returned as-is.
// Other providers surface exhausted quota responses (429, or 403/503
// with quota guidance) as the
// generic non-2xx response error instead, which callers used to classify as
// a plain network failure and therefore retried and notified as if quota had
// nothing to do with it; those are synthesized into the same typed error
// here, preserving the response's Retry-After / X-RateLimit-Reset metadata
// so the caller can back off until the quota window actually rolls over.
//
// The synthesized error leaves Provider empty: a non-2xx response error
// carries the endpoint but not the forge that produced it, and guessing one
// would be worse than saying "provider".
func AsRateLimitError(err error) (*RateLimitError, bool) {
	if err == nil {
		return nil, false
	}
	var rl *RateLimitError
	if errors.As(err, &rl) {
		return rl, true
	}
	var responseErr *providerResponseError
	if !errors.As(err, &responseErr) || !rateLimitResponse(responseErr.statusCode, responseErr.retryAfter, responseErr.rateLimitRemaining, responseErr.rateLimitReset) {
		return nil, false
	}
	limit := &RateLimitError{
		Endpoint:      responseErr.endpoint,
		Status:        responseErr.statusCode,
		RetryAfterRaw: responseErr.retryAfter,
		RemainingRaw:  responseErr.rateLimitRemaining,
		ResetRaw:      responseErr.rateLimitReset,
	}
	limit.Remaining, limit.RemainingKnown = parseRemainingHeader(responseErr.rateLimitRemaining)
	now := time.Now().UTC()
	if seconds, convErr := strconv.ParseInt(responseErr.rateLimitReset, 10, 64); convErr == nil && seconds > 0 {
		limit.Reset = time.Unix(seconds, 0).UTC()
	} else if delay, directed := retryAfterDelay(responseErr.retryAfter, now); directed {
		limit.Reset = now.Add(delay)
	}
	// Secondary stays false: it means GitHub's abuse/secondary limit
	// specifically, and no other forge distinguishes one from an exhausted
	// primary quota on a 429.
	return limit, true
}

// rateLimitResponse keeps ordinary permission failures and unavailable servers
// separate from responses that explicitly direct a quota wait.
func rateLimitResponse(status int, retryAfter, remaining, reset string) bool {
	if status == http.StatusTooManyRequests {
		return true
	}
	if status != http.StatusForbidden && status != http.StatusServiceUnavailable {
		return false
	}
	if strings.TrimSpace(retryAfter) != "" || strings.TrimSpace(remaining) == "0" {
		return true
	}
	// GitHub includes reset headers even on permission-denied responses.
	// A positive remaining quota must not turn those into endless waits.
	return strings.TrimSpace(reset) != "" && (status == http.StatusServiceUnavailable || strings.TrimSpace(remaining) == "")
}

// RetryDelay returns a conservative retry delay from all supplied quota
// guidance. An exhausted primary reset and Retry-After cannot shorten each
// other; an unexhausted primary quota does not delay a secondary retry.
// Missing/stale guidance uses a one-minute fallback, including secondary/abuse
// limits. Callers add jitter and bound the wait by their own deadline.
func (e *RateLimitError) RetryDelay(now time.Time) time.Duration {
	delay := time.Duration(0)
	// Secondary responses also carry the ordinary primary-quota reset.
	// When that quota still has capacity, it is not the retry deadline.
	if !e.Secondary || !e.RemainingKnown || e.Remaining == 0 {
		delay = e.Reset.Sub(now)
	}
	if retryAfter, ok := retryAfterDelay(e.RetryAfterRaw, now); ok && retryAfter > delay {
		delay = retryAfter
	}
	if delay <= 0 {
		return githubSecondaryFallbackMin
	}
	return delay
}

// parseRemainingHeader parses an X-RateLimit-Remaining value. Absent,
// malformed or negative values are reported as unknown (ok=false), never as a
// genuine remaining count of 0.
func parseRemainingHeader(raw string) (remaining int, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

func retryAfterDelay(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds >= 0 && seconds <= maxDurationSeconds {
		return time.Duration(seconds) * time.Second, true
	}
	at, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	delay := at.Sub(now)
	if delay < 0 {
		delay = 0
	}
	return delay, true
}

func fallbackBackoff(attempt int, jitter func(time.Duration) time.Duration) time.Duration {
	ceiling := backoffDuration(attempt)
	floor := ceiling / 2
	window := ceiling - floor
	if jitter == nil {
		return floor
	}
	offset := jitter(window)
	if offset < 0 {
		offset = 0
	}
	if offset > window {
		offset = window
	}
	return floor + offset
}

func randomJitter(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(max) + 1))
}

func backoffDuration(attempt int) time.Duration {
	d := rateLimitBackoffBase << attempt
	if d <= 0 || d > rateLimitBackoffMax {
		return rateLimitBackoffMax
	}
	return d
}

func rateLimitScope(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "unknown"
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	if u.Host == "" {
		return path
	}
	return u.Host + path
}
