package harness

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestEffectiveProcessTimeoutUsesSoonerParentDeadline(t *testing.T) {
	now := time.Now()
	ctx, cancel := context.WithDeadline(context.Background(), now.Add(90*time.Second))
	defer cancel()

	if got := effectiveProcessTimeout(ctx, 0, now); got != 90*time.Second {
		t.Fatalf("effective timeout = %s, want 90s parent deadline", got)
	}

	if got := effectiveProcessTimeout(ctx, 30*time.Second, now); got != 30*time.Second {
		t.Fatalf("effective timeout = %s, want 30s explicit timeout", got)
	}
}

func TestExecProcessRunnerRefusesCanceledContextBeforeStart(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(errors.New("budget exceeded"))
	_, err := (ExecProcessRunner{}).Run(ctx, ProcessRequest{Command: []string{"does-not-exist"}})
	if !errors.Is(err, ErrCanceled) {
		t.Fatalf("Run error = %v, want ErrCanceled before process start", err)
	}
}
