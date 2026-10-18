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
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("Err = %v, want deadline exceeded", ctx.Err())
	}
}

func TestWithTimeoutPreservesExplicitCancellation(t *testing.T) {
	ctx, cancel := WithTimeout(context.Background(), time.Second)
	cancel()
	<-ctx.Done()
	if !errors.Is(context.Cause(ctx), context.Canceled) {
		t.Fatalf("cause = %v, want canceled", context.Cause(ctx))
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("Err = %v, want canceled", ctx.Err())
	}
}

func TestSuspendedGapReportsWallTimeTheActiveClockMissed(t *testing.T) {
	for _, tc := range []struct {
		wall, active, want time.Duration
	}{
		{wall: 4*time.Hour + time.Minute, active: time.Minute, want: 4 * time.Hour},
		{wall: time.Minute + 30*time.Millisecond, active: time.Minute, want: 0}, // slew/granularity
		{wall: time.Minute, active: time.Minute + time.Hour, want: 0},           // wall clock stepped back
	} {
		if got := suspendedGap(tc.wall, tc.active); got != tc.want {
			t.Errorf("suspendedGap(%s, %s) = %s, want %s", tc.wall, tc.active, got, tc.want)
		}
	}
}

func TestWallMarkReportsNoSuspensionWithoutSleep(t *testing.T) {
	if got := NewWallMark().SuspendedSince(); got != 0 {
		t.Fatalf("SuspendedSince immediately after NewWallMark = %s, want 0", got)
	}
	if got := (WallMark{}).SuspendedSince(); got != 0 {
		t.Fatalf("zero WallMark reported %s suspended", got)
	}
}
