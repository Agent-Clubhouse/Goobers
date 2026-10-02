package providers

import (
	"context"
	"time"
)

type providerConstructorDefaults struct {
	maxRetries       int
	maxRateLimitWait time.Duration
	now              func() time.Time
	sleep            func(context.Context, time.Duration) error
	jitter           func(time.Duration) time.Duration
}

func newProviderConstructorDefaults() providerConstructorDefaults {
	return providerConstructorDefaults{
		maxRetries:       defaultRateLimitRetries,
		maxRateLimitWait: defaultRateLimitMaxWait,
		now:              time.Now,
		sleep:            contextSleep,
		jitter:           randomJitter,
	}
}

func (d providerConstructorDefaults) runtimeOrDefaults(
	now func() time.Time,
	sleep func(context.Context, time.Duration) error,
	jitter func(time.Duration) time.Duration,
) (func() time.Time, func(context.Context, time.Duration) error, func(time.Duration) time.Duration) {
	if now == nil {
		now = d.now
	}
	if sleep == nil {
		sleep = d.sleep
	}
	if jitter == nil {
		jitter = d.jitter
	}
	return now, sleep, jitter
}
