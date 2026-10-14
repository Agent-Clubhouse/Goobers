package workerhost

import (
	"context"
	"testing"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/interceptor"
)

type pendingCompletionActivity struct {
	interceptor.ActivityInboundInterceptorBase
}

func (*pendingCompletionActivity) ExecuteActivity(context.Context, *interceptor.ExecuteActivityInput) (interface{}, error) {
	return nil, activity.ErrResultPending
}

func TestActivityTrackerPreservesPendingCompletion(t *testing.T) {
	tracker := &activityTracker{buildID: "build", worker: "worker"}
	inbound := tracker.InterceptActivity(context.Background(), &pendingCompletionActivity{})
	_, err := inbound.ExecuteActivity(context.Background(), &interceptor.ExecuteActivityInput{})
	// The SDK compares this exact sentinel; wrapping it produces a second,
	// failed completion instead of allowing the task-bound completion owner.
	if err != activity.ErrResultPending { //nolint:errorlint // The SDK requires this exact sentinel, not an errors.Is match.
		t.Fatalf("pending completion was changed to %T: %v", err, err)
	}
	if tracker.inFlight() != 0 {
		t.Fatal("finished completion adapter retained its execution lease")
	}
}
