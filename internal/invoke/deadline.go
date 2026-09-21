package invoke

import (
	"context"
	"time"
)

type executionDeadlineKey struct{}

// WithExecutionDeadlineObserver attaches evidence of actual bounded execution.
// The callback returns cleanup for the same execution boundary.
func WithExecutionDeadlineObserver(ctx context.Context, observe func(time.Time) func()) context.Context {
	return context.WithValue(ctx, executionDeadlineKey{}, observe)
}

// BeginExecution observes the actual context deadline, never a configured
// duration. The returned context suppresses duplicate nested observations when
// an outer runtime already owns the whole bounded invocation.
func BeginExecution(ctx context.Context) (context.Context, func()) {
	done := func() {}
	if observe, ok := ctx.Value(executionDeadlineKey{}).(func(time.Time) func()); ok && observe != nil {
		if deadline, bounded := ctx.Deadline(); bounded {
			done = observe(deadline)
		}
	}
	return WithExecutionDeadlineObserver(ctx, nil), done
}
