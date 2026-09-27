package invoke

import (
	"context"
	"testing"
	"time"
)

func TestWholeExecutionBoundSuppressesNestedObservations(t *testing.T) {
	calls, finished := 0, 0
	deadline := time.Now().Add(time.Minute)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	ctx = WithExecutionDeadlineObserver(ctx, func(actual time.Time) func() {
		calls++
		if !actual.Equal(deadline) {
			t.Fatal(actual)
		}
		return func() { finished++ }
	})
	nested, done := BeginExecution(ctx)
	_, childDone := BeginExecution(nested)
	childDone()
	done()
	if calls != 1 || finished != 1 {
		t.Fatalf("calls=%d finished=%d", calls, finished)
	}
	unknown := WithExecutionDeadlineObserver(context.Background(), func(time.Time) func() { t.Fatal("unbounded context claimed bound"); return func() {} })
	_, unknownDone := BeginExecution(unknown)
	unknownDone()
}
