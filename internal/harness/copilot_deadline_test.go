package harness

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/invoke"
)

func TestControlledCopilotUsesWholeSessionDeadlineUntilClose(t *testing.T) {
	starts, finishes := 0, 0
	var actual time.Time
	ctx := invoke.WithExecutionDeadlineObserver(context.Background(), func(deadline time.Time) func() { starts++; actual = deadline; return func() { finishes++ } })
	runner := &copilotControlledRunner{factory: func(ctx context.Context, _ ProcessRequest, _ *copilotControlledRunner) (copilotModelSession, error) {
		_, nestedDone := invoke.BeginExecution(ctx)
		nestedDone()
		return nil, errors.New("synthetic setup failure")
	}}
	before := time.Now()
	_, _ = runner.Run(ctx, ProcessRequest{Timeout: time.Minute})
	if starts != 1 || finishes != 0 || actual.Before(before.Add(59*time.Second)) || actual.After(time.Now().Add(time.Minute)) {
		t.Fatalf("starts=%d finishes=%d deadline=%v", starts, finishes, actual)
	}
	runner.close()
	if finishes != 1 {
		t.Fatal("session deadline not cleared on close")
	}
}
