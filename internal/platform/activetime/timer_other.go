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
