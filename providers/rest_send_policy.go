package providers

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/goobers/goobers/internal/diagnostics/featureusage"
)

type restSendPolicy struct {
	client                   HTTPClient
	providerHTTPName         string
	maxTransientRetries      int
	maxRateLimitRetries      int
	maxRateLimitWait         time.Duration
	retryable                bool
	sleep                    func(context.Context, time.Duration) error
	decorate                 func(context.Context, *http.Request) error
	beforeSend               func(context.Context) error
	normalizeResponse        func(*http.Response)
	observeResponse          func(context.Context, *http.Response)
	refreshRejectedAuth      func() bool
	validateResponse         func(*http.Response, bool) error
	isRateLimited            func(*http.Response) bool
	planRateLimit            func(*http.Response, string, int) (time.Duration, RateLimitEvent)
	observeRateLimit         func(context.Context, RateLimitEvent)
	handleExhaustedRateLimit func(*http.Response, RateLimitEvent) (*http.Response, error)
}

// rateLimitWaitOutlivesDeadline reports whether sleeping wait for a rate limit
// would run past ctx's deadline (#5572). Such a sleep can only end in context
// cancellation — or, when the caller's deadline is the stage's, in the
// executor killing the stage as stage_timeout — so the request gives up at
// once with the typed rate-limit error, which carries the reset instant the
// scheduler defers on, instead of spending the rest of its budget asleep.
func rateLimitWaitOutlivesDeadline(ctx context.Context, wait time.Duration) bool {
	deadline, ok := ctx.Deadline()
	return ok && wait >= time.Until(deadline)
}

func sendJSONWithPolicy(ctx context.Context, policy restSendPolicy, method, endpoint string, body interface{}) (*http.Response, error) {
	maxWait := policy.maxRateLimitWait
	if maxWait <= 0 {
		maxWait = defaultRateLimitMaxWait
	}
	var rateLimitWaited time.Duration
	var rateLimitRetries, transientRetries int
	authRetried := false
	for {
		req, err := newJSONRequest(ctx, method, endpoint, body)
		if err != nil {
			return nil, err
		}
		if err := policy.decorate(ctx, req); err != nil {
			return nil, err
		}
		if policy.beforeSend != nil {
			if err := policy.beforeSend(ctx); err != nil {
				return nil, err
			}
		}
		featureusage.RecordProviderHTTP(policy.providerHTTPName)
		resp, err := httpClientOrDefault(policy.client).Do(req)
		if err != nil {
			if policy.retryable && transientRetries < policy.maxTransientRetries {
				if serr := policy.sleep(ctx, backoffDuration(transientRetries)); serr != nil {
					return nil, serr
				}
				transientRetries++
				continue
			}
			return nil, fmt.Errorf("send request: %w", err)
		}
		if policy.normalizeResponse != nil {
			policy.normalizeResponse(resp)
		}
		if policy.observeResponse != nil {
			policy.observeResponse(ctx, resp)
		}
		if resp.StatusCode == http.StatusUnauthorized && !authRetried &&
			policy.refreshRejectedAuth != nil && policy.refreshRejectedAuth() {
			_ = resp.Body.Close()
			authRetried = true
			continue
		}
		if policy.validateResponse != nil {
			if err := policy.validateResponse(resp, authRetried); err != nil {
				return nil, err
			}
		}
		if policy.isRateLimited(resp) {
			wait, ev := policy.planRateLimit(resp, endpoint, rateLimitRetries)
			if rateLimitRetries >= policy.maxRateLimitRetries || wait > maxWait-rateLimitWaited ||
				rateLimitWaitOutlivesDeadline(ctx, wait) {
				ev.Outcome = RateLimitOutcomeExhausted
				finalResp, finalErr := policy.handleExhaustedRateLimit(resp, ev)
				policy.observeRateLimit(ctx, ev)
				return finalResp, finalErr
			}
			_ = resp.Body.Close()
			if err := policy.sleep(ctx, wait); err != nil {
				ev.Outcome = RateLimitOutcomeCanceled
				policy.observeRateLimit(ctx, ev)
				return nil, err
			}
			ev.Outcome = RateLimitOutcomeRetry
			policy.observeRateLimit(ctx, ev)
			rateLimitWaited += wait
			rateLimitRetries++
			continue
		}
		if resp.StatusCode >= 500 && policy.retryable && transientRetries < policy.maxTransientRetries {
			_ = resp.Body.Close()
			if err := policy.sleep(ctx, backoffDuration(transientRetries)); err != nil {
				return nil, err
			}
			transientRetries++
			continue
		}
		return resp, nil
	}
}
