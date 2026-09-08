package worktree

import (
	"context"
	"errors"
	"slices"
	"testing"
)

func TestCleanupHandoffsComposeAndStopOnFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		manager := &Manager{}
		var calls []int
		blocked := errors.New("first handoff failed")
		WithBeforeCleanup(func(context.Context, CleanupTarget) error {
			calls = append(calls, 1)
			if fail {
				return blocked
			}
			return nil
		})(manager)
		WithBeforeCleanup(nil)(manager)
		WithBeforeCleanup(func(context.Context, CleanupTarget) error {
			calls = append(calls, 2)
			return nil
		})(manager)
		err := manager.prepareCleanup(context.Background(), "path", "stage", "owner")
		want := []int{1, 2}
		if fail {
			want = []int{1}
		}
		if !slices.Equal(calls, want) || errors.Is(err, blocked) != fail {
			t.Fatalf("handoff composition: calls=%v error=%v", calls, err)
		}
	}
}
