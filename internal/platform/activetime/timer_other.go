//go:build !windows

package activetime

import (
	"context"
	"time"
)

// WithTimeout preserves the standard context timeout implementation on
// platforms whose monotonic clock already has the desired semantics.
func WithTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, timeout)
}

// Mark records one instant so a later SuspendedSince can report how much of
// the elapsed time the host spent suspended. See the package documentation.
type Mark struct{}

// NewMark returns the zero Mark: the monotonic clock on these platforms
// already excludes host suspension, so there is nothing to subtract.
func NewMark() Mark {
	return Mark{}
}

// SuspendedSince always reports zero on platforms whose monotonic clock
// already excludes host suspension.
func (Mark) SuspendedSince() time.Duration {
	return 0
}

// WallMark records one instant so a later SuspendedSince can report how much
// wall-clock time since then the host spent suspended.
type WallMark struct {
	at time.Time
}

// NewWallMark records the current instant on both the wall clock and Go's
// monotonic clock, which on these platforms stops while the host sleeps.
func NewWallMark() WallMark {
	return WallMark{at: time.Now()}
}

// SuspendedSince reports how far the wall clock has advanced beyond the
// monotonic clock since m was taken: the time the host spent suspended. A
// forward wall-clock step larger than the noise floor reads the same way.
// The zero WallMark reports no suspension.
func (m WallMark) SuspendedSince() time.Duration {
	if m.at.IsZero() {
		return 0
	}
	now := time.Now()
	return suspendedGap(now.Round(0).Sub(m.at.Round(0)), now.Sub(m.at))
}
