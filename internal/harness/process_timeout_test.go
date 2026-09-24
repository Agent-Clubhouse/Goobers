package harness

import (
	"context"
	"testing"
	"time"
)

func TestEffectiveProcessDeadlineUsesSoonerParentDeadline(t *testing.T) {
	now := time.Now()
	ctx, cancel := context.WithDeadline(context.Background(), now.Add(90*time.Second))
	defer cancel()

	if got := effectiveProcessDeadline(ctx, 0, now); !got.Equal(now.Add(90 * time.Second)) {
		t.Fatalf("effective deadline = %s, want 90s parent deadline", got)
	}
	if got := effectiveProcessDeadline(ctx, 30*time.Second, now); !got.Equal(now.Add(30 * time.Second)) {
		t.Fatalf("effective deadline = %s, want 30s explicit timeout", got)
	}
}
