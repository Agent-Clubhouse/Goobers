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
