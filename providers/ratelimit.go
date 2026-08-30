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
)

// AsRateLimitError recovers a typed rate-limit error from err, provider-
// neutrally (#3647). A give-up *RateLimitError — the shape GitHub's send()
// returns once its in-request backoff budget is spent — is returned as-is.
// Every other provider (ADO, Gitea) surfaces an exhausted HTTP 429 as the
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
	if !errors.As(err, &responseErr) || responseErr.statusCode != http.StatusTooManyRequests {
		return nil, false
	}
	limit := &RateLimitError{
		Endpoint:      responseErr.endpoint,
		Status:        responseErr.statusCode,
		RetryAfterRaw: responseErr.retryAfter,
		RemainingRaw:  responseErr.rateLimitRemaining,
		ResetRaw:      responseErr.rateLimitReset,
	}
	if remaining, convErr := strconv.Atoi(responseErr.rateLimitRemaining); convErr == nil {
		limit.Remaining = remaining
	}
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

func retryAfterDelay(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	const maxDurationSeconds = (1<<63 - 1) / int64(time.Second)
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
