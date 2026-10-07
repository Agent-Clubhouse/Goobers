package planehttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/goobers/goobers/internal/retryutil"
)

// Transient-overload retry (#6890).
//
// The daemon sheds load at the door (admission control answers 503/429
// class_saturated before a handler runs), answers 503 recovering while it
// replays after a restart, and resets the stream when a handler blocks past
// its write deadline. Every one of those is momentary, and a stage that fails
// its whole run on one wastes the run's full budget. The retry below absorbs
// them within a bounded window and never masks a permanent error.
//
// Two classes of transient failure matter, because they differ in what a
// replay risks:
//
//   - Refused before any handler ran (admission 503/429, 503 recovering, a
//     dial failure, an HTTP/2 REFUSED_STREAM). The request had no effect, so
//     replaying it is safe for every method.
//   - Ambiguous (503 request_budget_exceeded, an HTTP/2 INTERNAL_ERROR stream
//     reset, a connection reset or EOF before a response). The handler may
//     have run and committed. Replaying is safe only for reads and for
//     mutations the caller declares replay-safe.

// Default retry bounds. The window is wall-clock from the first attempt and
// is further bounded by the caller's context, so a stage deadline always wins.
const (
	DefaultRetryMaxElapsed = 45 * time.Second
	DefaultRetryMaxTries   = 8
	DefaultRetryBase       = 250 * time.Millisecond
	DefaultRetryMaxDelay   = 5 * time.Second
	maxRetryAfter          = 5 * time.Second
	maxStatusBodyBytes     = 64 << 10
)

// RetryConfig bounds transient retries. Zero fields take the defaults.
type RetryConfig struct {
	// MaxElapsed is the wall-clock window after the first attempt begins in
	// which a further attempt may start.
	MaxElapsed time.Duration
	// MaxAttempts caps total attempts, including the first.
	MaxAttempts int
	// Backoff paces attempts; zero Base/Max take the defaults.
	Backoff retryutil.Policy
}

func (r RetryConfig) normalized() RetryConfig {
	if r.MaxElapsed <= 0 {
		r.MaxElapsed = DefaultRetryMaxElapsed
	}
	if r.MaxAttempts <= 0 {
		r.MaxAttempts = DefaultRetryMaxTries
	}
	if r.Backoff.Base <= 0 {
		r.Backoff.Base = DefaultRetryBase
	}
	if r.Backoff.Max <= 0 {
		r.Backoff.Max = DefaultRetryMaxDelay
	}
	return r
}

type transience int

const (
	notTransient transience = iota
	// refusedEarly: the daemon provably did not process the request.
	refusedEarly
	// ambiguous: the request may or may not have taken effect.
	ambiguous
)

// DoRetrying is DoRaw with bounded retry of transient overload. body is
// replayed from the byte slice on every attempt. Reads (GET, HEAD) are always
// replayed; any other method is replayed after an ambiguous failure only when
// replaySafe is true — the caller's assertion that the route is idempotent,
// lease- or CAS-guarded, or deduplicated by a request key. A request refused
// before a handler ran is replayed regardless.
func (c *Client) DoRetrying(ctx context.Context, method, path string, body []byte, headers http.Header, replaySafe bool) (*http.Response, error) {
	cfg := c.retry.normalized()
	replayAmbiguous := replaySafe || method == http.MethodGet || method == http.MethodHead
	start := time.Now()
	for attempt := 0; ; attempt++ {
		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(body)
		}
		response, err := c.DoRaw(ctx, method, path, reader, headers)
		kind := notTransient
		var retryAfter time.Duration
		if err != nil {
			var requestErr *RequestError
			if errors.As(err, &requestErr) && requestErr.Op == "do" {
				kind = classifyTransportError(requestErr.Err)
			}
		} else if response.StatusCode == http.StatusServiceUnavailable || response.StatusCode == http.StatusTooManyRequests {
			raw, readErr := ReadBounded(response.Body, maxStatusBodyBytes)
			_ = response.Body.Close()
			response.Body = io.NopCloser(bytes.NewReader(raw))
			if readErr == nil {
				kind = classifyStatus(response.StatusCode, raw)
				retryAfter = parseRetryAfter(response.Header.Get("Retry-After"))
			}
		}
		if kind == notTransient || (kind == ambiguous && !replayAmbiguous) {
			return response, err
		}
		delay := retryutil.JitteredExponential(cfg.Backoff, attempt)
		if retryAfter > delay {
			delay = retryAfter
		}
		if ctx.Err() != nil || attempt+1 >= cfg.MaxAttempts || time.Since(start)+delay >= cfg.MaxElapsed {
			return exhausted(response, err, attempt+1)
		}
		if response != nil {
			_ = response.Body.Close()
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return exhausted(response, err, attempt+1)
		case <-timer.C:
		}
	}
}

// exhausted reports the last failure. A status response is returned as is (its
// body was buffered) so the caller decodes its typed refusal; a transport error
// is annotated with the attempt count.
func exhausted(response *http.Response, err error, attempts int) (*http.Response, error) {
	if response != nil {
		return response, nil
	}
	var requestErr *RequestError
	if errors.As(err, &requestErr) {
		return nil, &RequestError{Op: requestErr.Op, Endpoint: requestErr.Endpoint,
			Err: fmt.Errorf("%w (gave up after %d attempt(s) on a transient failure)", requestErr.Err, attempts)}
	}
	return nil, err
}

// DoJSONRetrying is DoJSON with the retry of DoRetrying.
func (c *Client) DoJSONRetrying(ctx context.Context, method, path string, body any, headers http.Header, replaySafe bool) (*http.Response, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, &RequestError{Op: "encode", Endpoint: c.baseURL + path, Err: err}
	}
	if headers == nil {
		headers = make(http.Header)
	} else {
		headers = headers.Clone()
	}
	headers.Set("Content-Type", "application/json")
	return c.DoRetrying(ctx, method, path, payload, headers, replaySafe)
}

func parseRetryAfter(value string) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || seconds <= 0 {
		return 0
	}
	return min(time.Duration(seconds)*time.Second, maxRetryAfter)
}

// classifyStatus decides whether an overload response is transient.
func classifyStatus(status int, raw []byte) transience {
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &envelope)
	code := envelope.Error.Code
	switch {
	case status == http.StatusTooManyRequests && (code == "class_saturated" || code == ""):
		// Admission shedding declines before any handler runs. A 429 carrying
		// some other code is a different refusal and is not assumed transient.
		return refusedEarly
	case status == http.StatusServiceUnavailable && (code == "class_saturated" || code == "recovering"):
		return refusedEarly
	case status == http.StatusServiceUnavailable && code == "request_budget_exceeded":
		// The handler ran long enough to hit its budget; it may still commit.
		return ambiguous
	}
	return notTransient
}

// classifyTransportError decides whether a transport failure is transient.
func classifyTransportError(err error) transience {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return notTransient
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		// A timed-out attempt already spent its budget; do not spend another.
		return notTransient
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return refusedEarly
	}
	message := err.Error()
	// The stdlib bundles its own HTTP/2, whose StreamError is not the x/net
	// type, so the stable text form is the discriminant.
	if strings.Contains(message, "stream error:") {
		switch {
		case strings.Contains(message, "REFUSED_STREAM"):
			return refusedEarly
		case strings.Contains(message, "INTERNAL_ERROR"):
			return ambiguous
		}
		return notTransient
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) ||
		strings.Contains(message, "connection reset by peer") {
		return ambiguous
	}
	return notTransient
}
