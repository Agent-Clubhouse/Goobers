package activetime

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestWithTimeoutPreservesDeadlineAndCause(t *testing.T) {
	ctx, cancel := WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if _, ok := ctx.Deadline(); !ok {
		t.Fatal("active-time context has no deadline")
	}
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("active-time context did not expire")
	}
	if !errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
		t.Fatalf("cause = %v, want deadline exceeded", context.Cause(ctx))
	}
}

func TestWithTimeoutPreservesExplicitCancellation(t *testing.T) {
	ctx, cancel := WithTimeout(context.Background(), time.Second)
	cancel()
	<-ctx.Done()
	if !errors.Is(context.Cause(ctx), context.Canceled) {
		t.Fatalf("cause = %v, want canceled", context.Cause(ctx))
	}
}
