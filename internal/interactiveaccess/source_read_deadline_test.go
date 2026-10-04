package interactiveaccess

import (
	"context"
	"errors"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestSourceReadAndViewBoundPolicyLockWait(t *testing.T) {
	for _, operation := range []string{"read", "view"} {
		t.Run(operation, func(t *testing.T) {
			s, _ := testService(t, testGaggle(), testSources())
			called := make(chan struct{}, 1)
			invoke := func(ctx context.Context) error {
				if operation == "read" {
					return s.WithSourceRead(ctx, testPrincipal(), "web", "backlog.read", func(context.Context, *apiv1.Gaggle, SourceCredentialLoader) error {
						called <- struct{}{}
						return nil
					})
				}
				return s.WithSourceView(ctx, testPrincipal(), "web", func(context.Context, *apiv1.Gaggle) error {
					called <- struct{}{}
					return nil
				})
			}
			s.mu.Lock()
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- invoke(ctx) }()
			select {
			case err := <-done:
				s.mu.Unlock()
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				s.mu.Unlock()
				<-done
				t.Fatal("source operation waited past its deadline for applied policy")
			}
			if err := invoke(ctx); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("expired operation entered the unlocked policy snapshot", err)
			}
			select {
			case <-called:
				t.Fatal("expired operation invoked source callback")
			default:
			}
		})
	}
}
