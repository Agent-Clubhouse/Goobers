package harness

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/credentials"
)

// MaxTransientPreflightRetryAfter caps a provider-requested wait read from a
// probe's output, so a garbled or hostile Retry-After cannot park a harness
// for longer than a provider quota window.
const MaxTransientPreflightRetryAfter = time.Hour

// TransientPreflightError marks a harness sign-in probe that failed because
// the model provider could not answer at that moment (#6017): a rate-limit
// 403 (X-RateLimit-Remaining: 0, Retry-After, "API rate limit exceeded"), a
// 429, a 5xx, or a network error. Probing the same credential again later can
// succeed, so callers treat it as a retryable infrastructure failure, not as
// a missing sign-in. errors.Is(err, credentials.ErrTransientProvider) holds,
// the same marker the identity-verification path uses (#5596).
type TransientPreflightError struct {
	Err error
	// RetryAfter is the wait the provider asked for, read from the probe's
	// output; zero when it named none.
	RetryAfter time.Duration
}

func (e *TransientPreflightError) Error() string { return e.Err.Error() }
func (e *TransientPreflightError) Unwrap() error { return e.Err }

// Is reports the credentials.ErrTransientProvider marker.
func (e *TransientPreflightError) Is(target error) bool {
	return target == credentials.ErrTransientProvider
}

// TransientPreflightRetryAfter returns the provider-requested wait carried by
// a transient preflight failure anywhere in err's chain.
func TransientPreflightRetryAfter(err error) (time.Duration, bool) {
	var transient *TransientPreflightError
	if !errors.As(err, &transient) || transient.RetryAfter <= 0 {
		return 0, false
	}
	return transient.RetryAfter, true
}

// transientAuthProbeHint is the remediation a transient probe failure names.
const transientAuthProbeHint = "the model provider is rate limiting or unreachable; the probe is retried with backoff"

// transientProbeFailure classifies a failed sign-in probe from its output. It
// returns a *TransientPreflightError when the output carries a transient
// provider signal and no credential-rejection signal, else nil. A timeout or
// cancellation is reported by the caller as such and is never reclassified.
func transientProbeFailure(probe string, result ProcessResult, runErr error) error {
	if errors.Is(runErr, ErrTimeout) || errors.Is(runErr, ErrCanceled) {
		return nil
	}
	transient, retryAfter := classifyTransientProbeOutput(result.Transcript)
	if !transient {
		return nil
	}
	return &TransientPreflightError{
		Err:        preflightProbeError(probe, result, runErr, transientAuthProbeHint),
		RetryAfter: retryAfter,
	}
}

// authProbeFailure is the error for a failed sign-in probe: transient when
// its output says the provider could not answer, else the ordinary probe
// failure with hint.
func authProbeFailure(probe string, result ProcessResult, runErr error, hint string) error {
	if err := transientProbeFailure(probe, result, runErr); err != nil {
		return err
	}
	return preflightProbeError(probe, result, runErr, hint)
}

var (
	// A credential the provider REJECTED fails closed as a sign-in problem
	// even when the same output also mentions a transient condition.
	probeCredentialRejected = regexp.MustCompile(`\b401\b|unauthorized|bad credentials|invalid or revoked|not signed in|not logged in|not authenticated|signed out|\bexpired\b`)
	probeRateLimited        = regexp.MustCompile(`rate[ -]limit(ed)? exceeded|api rate limit|secondary rate limit|rate[ -]limited|too many requests|abuse detection`)
	probeRateLimitExhausted = regexp.MustCompile(`x-ratelimit-remaining["']?\s*[:=]\s*["']?0\b`)
	probeRetryAfter         = regexp.MustCompile(`retry-after["']?\s*[:=]\s*["']?(\d+)|retry after (\d+)\s*(?:s|sec|secs|seconds)\b`)
	probeServerStatus       = regexp.MustCompile(`\((429|5\d\d)\)|\b(?:status|http|status code|statuscode)["']?\s*[:=]?\s*(429|5\d\d)\b|internal server error|bad gateway|service unavailable|gateway timeout`)
	probeNetworkFailure     = regexp.MustCompile(`econnreset|econnrefused|etimedout|enotfound|eai_again|enetunreach|ehostunreach|socket hang up|getaddrinfo|network error|fetch failed|connection reset by peer|tls handshake timeout|i/o timeout|no such host`)
)

// classifyTransientProbeOutput reports whether a sign-in probe's output names
// a transient provider condition, and the Retry-After it carried. Matching is
// on the probe's own output because the CLI reports the provider response
// only there; the signals mirror credentials' HTTP classification (429, 5xx,
// a 403 with the primary or secondary rate-limit signal, a transport error).
// A plain 403 is not one of them and stays a sign-in failure.
func classifyTransientProbeOutput(transcript []byte) (bool, time.Duration) {
	text := strings.ToLower(string(transcript))
	if text == "" || probeCredentialRejected.MatchString(text) {
		return false, 0
	}
	retryAfter, hasRetryAfter := probeRetryAfterSeconds(text)
	transient := hasRetryAfter ||
		probeRateLimited.MatchString(text) ||
		probeRateLimitExhausted.MatchString(text) ||
		probeServerStatus.MatchString(text) ||
		probeNetworkFailure.MatchString(text)
	if !transient {
		return false, 0
	}
	return true, retryAfter
}

func probeRetryAfterSeconds(text string) (time.Duration, bool) {
	match := probeRetryAfter.FindStringSubmatch(text)
	if match == nil {
		return 0, false
	}
	digits := match[1]
	if digits == "" {
		digits = match[2]
	}
	seconds, err := strconv.ParseInt(digits, 10, 64)
	if err != nil || seconds > int64(MaxTransientPreflightRetryAfter/time.Second) {
		return MaxTransientPreflightRetryAfter, true
	}
	return time.Duration(seconds) * time.Second, true
}
