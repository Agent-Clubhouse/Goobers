package worktree

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
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
		if !slices.Equal(calls, want) || errors.Is(err, blocked) != fail || errors.Is(err, ErrCleanupDeferred) != fail {
			t.Fatalf("handoff composition: calls=%v error=%v", calls, err)
		}
	}
}

func TestNamedCleanupFailurePreservesCauseAndDefers(t *testing.T) {
	manager := &Manager{}
	cause := errors.New("journal unavailable")
	if err := manager.SetCleanupGuard("recovery", func(context.Context, CleanupTarget) error { return cause }); err != nil {
		t.Fatal(err)
	}
	err := manager.prepareCleanup(context.Background(), "path", "stage", "owner")
	if !errors.Is(err, ErrCleanupDeferred) || !errors.Is(err, cause) {
		t.Fatalf("cleanup lost deferred classification or original cause: %v", err)
	}
}

func TestNamedCleanupGuardReplacementPreservesOtherHandoffs(t *testing.T) {
	manager := &Manager{}
	var calls []string
	WithBeforeCleanup(func(context.Context, CleanupTarget) error { calls = append(calls, "original"); return nil })(manager)
	for _, name := range []string{"provenance", "recovery", "recovery"} {
		if err := manager.SetCleanupGuard(name, func(context.Context, CleanupTarget) error { calls = append(calls, name); return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if err := manager.prepareCleanup(context.Background(), "path", "stage", "owner"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(calls, []string{"original", "provenance", "recovery"}) {
		t.Fatalf("reload replaced unrelated guards or duplicated recovery: %v", calls)
	}
}

func TestNamedCleanupGuardConcurrentReplacement(t *testing.T) {
	manager := &Manager{}
	var calls atomic.Int64
	callback := func(context.Context, CleanupTarget) error { calls.Add(1); return nil }
	if err := manager.SetCleanupGuard("recovery", callback); err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	workers.Go(func() {
		for range 100 {
			if err := manager.SetCleanupGuard("recovery", callback); err != nil {
				t.Error(err)
			}
		}
	})
	workers.Go(func() {
		for range 100 {
			if err := manager.prepareCleanup(context.Background(), "path", "stage", "owner"); err != nil {
				t.Error(err)
			}
		}
	})
	workers.Wait()
	if calls.Load() != 100 {
		t.Fatalf("reload duplicated or lost cleanup acknowledgements: %d", calls.Load())
	}
}
