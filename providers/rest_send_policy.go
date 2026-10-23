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
	state := restSendState{maxRateLimitWait: maxWait}
	for {
		req, err := preparePolicyRequest(ctx, policy, method, endpoint, body)
		if err != nil {
			return nil, err
		}
		featureusage.RecordProviderHTTP(policy.providerHTTPName)
		resp, err := httpClientOrDefault(policy.client).Do(req)
		if err != nil {
			retry, err := retryPolicyTransportError(ctx, policy, &state, err)
			if retry {
				continue
			}
			return nil, err
		}
		observePolicyResponse(ctx, policy, resp)
		if resp.StatusCode == http.StatusUnauthorized && !state.authRetried &&
			policy.refreshRejectedAuth != nil && policy.refreshRejectedAuth() {
			_ = resp.Body.Close()
			state.authRetried = true
			continue
		}
		if policy.validateResponse != nil {
			if err := policy.validateResponse(resp, state.authRetried); err != nil {
				return nil, err
			}
		}
		if policy.isRateLimited(resp) {
			finalResp, retry, err := handlePolicyRateLimit(ctx, policy, endpoint, resp, &state)
			if retry {
				continue
			}
			return finalResp, err
		}
		if resp.StatusCode >= 500 && policy.retryable && state.transientRetries < policy.maxTransientRetries {
			_ = resp.Body.Close()
			if err := policy.sleep(ctx, backoffDuration(state.transientRetries)); err != nil {
				return nil, err
			}
			state.transientRetries++
			continue
		}
		return resp, nil
	}
}

type restSendState struct {
	maxRateLimitWait time.Duration
	rateLimitWaited  time.Duration
	rateLimitRetries int
	transientRetries int
	authRetried      bool
}

func preparePolicyRequest(
	ctx context.Context, policy restSendPolicy, method, endpoint string, body interface{},
) (*http.Request, error) {
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
	return req, nil
}

func retryPolicyTransportError(
	ctx context.Context, policy restSendPolicy, state *restSendState, sendErr error,
) (bool, error) {
	if isRequestAdmissionError(sendErr) || !policy.retryable || state.transientRetries >= policy.maxTransientRetries {
		return false, fmt.Errorf("send request: %w", sendErr)
	}
	if err := policy.sleep(ctx, backoffDuration(state.transientRetries)); err != nil {
		return false, err
	}
	state.transientRetries++
	return true, nil
}

func observePolicyResponse(ctx context.Context, policy restSendPolicy, resp *http.Response) {
	if policy.normalizeResponse != nil {
		policy.normalizeResponse(resp)
	}
	if policy.observeResponse != nil {
		policy.observeResponse(ctx, resp)
	}
}

func handlePolicyRateLimit(
	ctx context.Context,
	policy restSendPolicy,
	endpoint string,
	resp *http.Response,
	state *restSendState,
) (*http.Response, bool, error) {
	wait, event := policy.planRateLimit(resp, endpoint, state.rateLimitRetries)
	if state.rateLimitRetries >= policy.maxRateLimitRetries ||
		wait > state.maxRateLimitWait-state.rateLimitWaited ||
		rateLimitWaitOutlivesDeadline(ctx, wait) {
		event.Outcome = RateLimitOutcomeExhausted
		finalResp, err := policy.handleExhaustedRateLimit(resp, event)
		policy.observeRateLimit(ctx, event)
		return finalResp, false, err
	}
	_ = resp.Body.Close()
	if err := policy.sleep(ctx, wait); err != nil {
		event.Outcome = RateLimitOutcomeCanceled
		policy.observeRateLimit(ctx, event)
		return nil, false, err
	}
	event.Outcome = RateLimitOutcomeRetry
	policy.observeRateLimit(ctx, event)
	state.rateLimitWaited += wait
	state.rateLimitRetries++
	return nil, true, nil
}
