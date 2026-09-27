package credentials

import (
	"context"
	"strings"
	"time"
)

// MinDeliveredLifetime is the least remaining lifetime a credential with a
// stated expiry should have when the daemon hands it to a stage, locally or
// through the credential plane for a stage pod (#5905).
//
// A stage cannot refresh what it was delivered: a Microsoft Entra token or a
// GitHub App installation token lives about an hour, and a source cache
// refreshes only a few minutes before expiry, so without a floor a stage could
// start with a value that lapses mid-stage. Twenty minutes covers a
// deterministic stage with room to spare while still reusing a cached token
// for most of its life. A value that still has less than this left after a
// refresh (a source whose own cache returns the same token) is delivered
// anyway, with its expiry, rather than failing the stage.
const MinDeliveredLifetime = 20 * time.Minute

// WithMinimumLifetime returns a source that asks refresh for a new value when
// the value f returns states an expiry less than MinDeliveredLifetime away.
// refresh must bypass whatever cache f reads (re-mint, rebuild the source).
// A value with no stated expiry is returned as is.
//
// The refresh is best effort. When it fails, or returns a value that expires
// sooner than the one already held, the held value is returned: it is still
// valid, and delivering it is what happened before this floor existed. A
// value that has already expired is never returned in place of a refresh
// error.
func (f ExpiringResolveFunc) WithMinimumLifetime(refresh ExpiringResolveFunc) ExpiringResolveFunc {
	return f.WithMinimumLifetimeClock(refresh, time.Now)
}

// WithMinimumLifetimeClock is WithMinimumLifetime measured against now, for a
// source that keeps its own clock (a nil now uses time.Now).
func (f ExpiringResolveFunc) WithMinimumLifetimeClock(refresh ExpiringResolveFunc, now func() time.Time) ExpiringResolveFunc {
	if now == nil {
		now = time.Now
	}
	return withMinimumLifetime(f, refresh, now)
}

func withMinimumLifetime(f, refresh ExpiringResolveFunc, now func() time.Time) ExpiringResolveFunc {
	if refresh == nil {
		return f
	}
	return func(ctx context.Context) (string, time.Time, error) {
		value, expiresAt, err := f(ctx)
		if err != nil || expiresAt.IsZero() {
			return value, expiresAt, err
		}
		at := now()
		if !expiresAt.Before(at.Add(MinDeliveredLifetime)) {
			return value, expiresAt, nil
		}
		fresh, freshExpiry, refreshErr := refresh(ctx)
		if refreshErr != nil || strings.TrimSpace(fresh) == "" {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return "", time.Time{}, ctxErr
			}
			if !expiresAt.After(at) {
				if refreshErr == nil {
					return "", time.Time{}, ErrTokenRefEmpty
				}
				return "", time.Time{}, refreshErr
			}
			return value, expiresAt, nil
		}
		if !freshExpiry.IsZero() && freshExpiry.Before(expiresAt) {
			return value, expiresAt, nil
		}
		return fresh, freshExpiry, nil
	}
}
