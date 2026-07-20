package providers

import (
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
