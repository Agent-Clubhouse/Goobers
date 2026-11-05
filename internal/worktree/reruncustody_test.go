package worktree

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRerunCustodyHonorsRequestBudgetUnderRepositoryContention(t *testing.T) {
	manager := newTestManager(t)
	const source = "local-test-source"
	held := manager.lockFor(repoKey(source))
	held.Lock()
	defer held.Unlock()
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	done := make(chan error, 1)
	go func() { done <- manager.VerifyRunBranch(canceled, source, "goobers/work/run", false) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("already-canceled admission: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("already-canceled admission waited on repository mutex")
	}
	for _, check := range []struct {
		name string
		run  func(context.Context) error
	}{
		{"branch", func(ctx context.Context) error {
			return manager.VerifyRunBranch(ctx, source, "goobers/work/run", false)
		}},
		{"child-mirror", func(ctx context.Context) error {
			_, err := manager.WithExistingMirror(ctx, source, func(string) error { t.Error("entered contended mirror"); return nil })
			return err
		}},
	} {
		t.Run(check.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- check.run(ctx) }()
			select {
			case err := <-done:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("custody error=%v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("custody preflight outlived request budget")
			}
		})
	}
}
