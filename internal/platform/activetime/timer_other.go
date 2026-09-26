//go:build !windows

package activetime

import (
	"context"
	"time"
)

func newTimer(timeout time.Duration) *Timer {
	timer := time.NewTimer(timeout)
	return &Timer{C: timer.C, stop: timer.Stop}
}

// WithTimeout preserves the standard context timeout implementation on
// platforms whose monotonic clock already has the desired semantics.
func WithTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, timeout)
}
