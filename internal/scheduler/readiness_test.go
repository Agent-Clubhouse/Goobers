package scheduler

import (
	"context"
	"testing"
)

func TestConcurrencyLimiter(t *testing.T) {
	ev := Event{WorkflowName: "flow"}
	ctx := context.Background()

	// Unlimited when Max <= 0.
	if ready, _, _ := (ConcurrencyLimiter{Max: 0}).Ready(ctx, ev); !ready {
		t.Error("Max=0 should be unlimited")
	}
	// Under the limit.
	if ready, _, _ := (ConcurrencyLimiter{Max: 2, Active: func(string) int { return 1 }}).Ready(ctx, ev); !ready {
		t.Error("1/2 active should be ready")
	}
	// At the limit blocks.
	ready, reason, _ := (ConcurrencyLimiter{Max: 2, Active: func(string) int { return 2 }}).Ready(ctx, ev)
	if ready {
		t.Error("2/2 active should block")
	}
	if reason == "" {
		t.Error("blocked condition should give a reason")
	}
	// Nil Active counts as zero.
	if ready, _, _ := (ConcurrencyLimiter{Max: 1}).Ready(ctx, ev); !ready {
		t.Error("nil Active should be treated as 0 active")
	}
}

func TestReadinessFunc(t *testing.T) {
	called := false
	f := ReadinessFunc{Label: "dep", Fn: func(context.Context, Event) (bool, string, error) {
		called = true
		return true, "", nil
	}}
	if f.Name() != "dep" {
		t.Errorf("name = %q, want dep", f.Name())
	}
	if ready, _, _ := f.Ready(context.Background(), Event{}); !ready || !called {
		t.Error("ReadinessFunc should delegate to Fn")
	}
	// Default label.
	if (ReadinessFunc{}).Name() != "readiness" {
		t.Error("empty label should default to 'readiness'")
	}
}
