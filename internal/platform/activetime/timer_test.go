package activetime

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestTimerFiresAfterActiveDuration(t *testing.T) {
	timer := NewTimer(25 * time.Millisecond)
	defer timer.Stop()

	select {
	case <-timer.C:
	case <-time.After(2 * time.Second):
		t.Fatal("active-time timer did not fire")
	}
}

func TestTimerStopPreventsFiring(t *testing.T) {
	timer := NewTimer(time.Second)
	if !timer.Stop() {
		t.Fatal("first Stop returned false")
	}
	if timer.Stop() {
		t.Fatal("second Stop returned true")
	}

	select {
	case <-timer.C:
		t.Fatal("stopped active-time timer fired")
	case <-time.After(50 * time.Millisecond):
	}
}

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
