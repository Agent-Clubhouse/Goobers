package credentials

import (
	"context"
	"strings"
	"sync"
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

// LifetimeFloor configures ExpiringResolveFunc.WithLifetimeFloor.
type LifetimeFloor struct {
	// Refresh returns a new value, bypassing whatever cache the wrapped
	// source reads (re-mint, rebuild the source). Nil disables the floor.
	Refresh ExpiringResolveFunc
	// Now is the clock the remaining lifetime is measured against. Nil uses
	// time.Now.
	Now func() time.Time
	// OnShortDelivery, when set, is told when a still-valid value below the
	// floor is delivered because the refresh failed (err is the refresh
	// error) or could not lift it above the floor (err is nil). It receives
	// the delivered value's expiry, never the value.
	OnShortDelivery func(expiresAt time.Time, err error)
}

// WithMinimumLifetime returns a source that asks refresh for a new value when
// the value f returns states an expiry less than MinDeliveredLifetime away.
// It is WithLifetimeFloor with only Refresh set.
func (f ExpiringResolveFunc) WithMinimumLifetime(refresh ExpiringResolveFunc) ExpiringResolveFunc {
	return f.WithLifetimeFloor(LifetimeFloor{Refresh: refresh})
}

// WithLifetimeFloor returns a source that asks floor.Refresh for a new value
// when the value f returns states an expiry less than MinDeliveredLifetime
// away. A value with no stated expiry is returned as is.
//
// The refresh is best effort. When it fails, or returns a value that expires
// sooner than the one already held, the held value is returned: it is still
// valid, and delivering it is what happened before this floor existed. A
// value that has already expired is never returned in place of a refresh
// error.
//
// A refresh that succeeds but leaves the delivered value below the floor (an
// upstream cache that hands back the same token) is not repeated for that
// value: it is delivered as is until the source's own cache moves on, rather
// than paying a refresh on every resolve.
func (f ExpiringResolveFunc) WithLifetimeFloor(floor LifetimeFloor) ExpiringResolveFunc {
	if floor.Refresh == nil {
		return f
	}
	if floor.Now == nil {
		floor.Now = time.Now
	}
	w := &lifetimeFloor{source: f, floor: floor}
	return w.resolve
}

// lifetimeFloor is the state behind WithLifetimeFloor: the expiry of the last
// value a successful refresh could not lift above the floor.
type lifetimeFloor struct {
	source ExpiringResolveFunc
	floor  LifetimeFloor

	mu         sync.Mutex
	unimproved time.Time
}

func (w *lifetimeFloor) resolve(ctx context.Context) (string, time.Time, error) {
	value, expiresAt, err := w.source(ctx)
	if err != nil || expiresAt.IsZero() {
		return value, expiresAt, err
	}
	at := w.floor.Now()
	if !expiresAt.Before(at.Add(MinDeliveredLifetime)) {
		return value, expiresAt, nil
	}
	if expiresAt.After(at) && w.refreshCannotLift(expiresAt) {
		return value, expiresAt, nil
	}
	fresh, freshExpiry, refreshErr := w.floor.Refresh(ctx)
	if refreshErr != nil || strings.TrimSpace(fresh) == "" {
		return w.fallback(ctx, value, expiresAt, at, refreshErr)
	}
	if !freshExpiry.IsZero() && freshExpiry.Before(expiresAt) {
		value, expiresAt = w.shortDelivery(value, expiresAt)
		return value, expiresAt, nil
	}
	if !freshExpiry.IsZero() && freshExpiry.Before(at.Add(MinDeliveredLifetime)) {
		fresh, freshExpiry = w.shortDelivery(fresh, freshExpiry)
	}
	return fresh, freshExpiry, nil
}

// shortDelivery records that a successful refresh left value below the floor,
// so the same value is not refreshed again, and reports it once.
func (w *lifetimeFloor) shortDelivery(value string, expiresAt time.Time) (string, time.Time) {
	w.mu.Lock()
	w.unimproved = expiresAt
	w.mu.Unlock()
	w.observe(expiresAt, nil)
	return value, expiresAt
}

// fallback handles a refresh that failed or returned nothing: a still-valid
// held value is delivered (and reported), an expired one is not.
func (w *lifetimeFloor) fallback(ctx context.Context, value string, expiresAt, at time.Time, refreshErr error) (string, time.Time, error) {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", time.Time{}, ctxErr
	}
	if refreshErr == nil {
		refreshErr = ErrTokenRefEmpty
	}
	if !expiresAt.After(at) {
		return "", time.Time{}, refreshErr
	}
	w.observe(expiresAt, refreshErr)
	return value, expiresAt, nil
}

func (w *lifetimeFloor) refreshCannotLift(expiresAt time.Time) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.unimproved.Equal(expiresAt)
}

func (w *lifetimeFloor) observe(expiresAt time.Time, err error) {
	if w.floor.OnShortDelivery != nil {
		w.floor.OnShortDelivery(expiresAt, err)
	}
}
