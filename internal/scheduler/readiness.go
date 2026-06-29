package scheduler

import (
	"context"
	"fmt"
)

// ReadinessCondition gates whether a triggered workflow may actually start. All
// conditions on a scheduler must pass. Reason is a short human-readable
// explanation returned when ready is false.
type ReadinessCondition interface {
	Name() string
	Ready(ctx context.Context, ev Event) (ready bool, reason string, err error)
}

// ReadinessFunc adapts a function to a ReadinessCondition (e.g. a dependency
// check: "workflow X must have completed").
type ReadinessFunc struct {
	Label string
	Fn    func(ctx context.Context, ev Event) (bool, string, error)
}

// Name returns the condition's label.
func (f ReadinessFunc) Name() string {
	if f.Label == "" {
		return "readiness"
	}
	return f.Label
}

// Ready delegates to the wrapped function.
func (f ReadinessFunc) Ready(ctx context.Context, ev Event) (bool, string, error) {
	return f.Fn(ctx, ev)
}

// ConcurrencyLimiter blocks a workflow's runs once Max are already active. Active
// reports the current number of in-flight runs for a workflow (e.g. queried from
// the engine/Temporal). A Max <= 0 means unlimited.
type ConcurrencyLimiter struct {
	Max    int
	Active func(workflowName string) int
}

// Name identifies the condition.
func (l ConcurrencyLimiter) Name() string { return "concurrency-limit" }

// Ready reports whether another run may start under the concurrency limit.
func (l ConcurrencyLimiter) Ready(_ context.Context, ev Event) (bool, string, error) {
	if l.Max <= 0 {
		return true, "", nil
	}
	active := 0
	if l.Active != nil {
		active = l.Active(ev.WorkflowName)
	}
	if active >= l.Max {
		return false, fmt.Sprintf("%d/%d runs active", active, l.Max), nil
	}
	return true, "", nil
}
