package providers

import (
	"context"
	"net/http"
)

type noMutationRetryKey struct{}

// WithoutMutationRetries disables all transport, rate-limit and refreshed-auth
// replay of non-read REST methods. It is for human commands whose preflight
// revision must not be reused after an uncertain write. GET/HEAD remain bounded
// by ordinary read retry policy. Native adapters always apply this internally.
func WithoutMutationRetries(ctx context.Context) context.Context {
	return context.WithValue(ctx, noMutationRetryKey{}, true)
}

func nativeMutationPolicy(ctx context.Context, method string, policy restSendPolicy) restSendPolicy {
	noRetry, _ := ctx.Value(noMutationRetryKey{}).(bool)
	if !noRetry || method == http.MethodGet || method == http.MethodHead {
		return policy
	}
	policy.retryable = false
	policy.maxTransientRetries = 0
	policy.maxRateLimitRetries = 0
	policy.refreshRejectedAuth = nil
	return policy
}
