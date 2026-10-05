package interactiveaccess

import (
	"context"
	"errors"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestSourceWriteDeadlineIncludesPolicyLockWait(t *testing.T) {
	s, _ := testService(t, testGaggle(), testSources())
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- s.WithSourceWrite(ctx, testPrincipal(), "web", func(context.Context, *apiv1.Gaggle, SourceCredentialLoader) error {
			return errors.New("expired request reached command acceptance")
		})
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("write admission ignored its deadline while waiting for policy reload")
	}
}

func TestSourceWriteSelectionDoesNotResolveCredentialForReceipts(t *testing.T) {
	t.Setenv("HUMAN_ISSUES_TOKEN", "")
	s, registrar := testService(t, testGaggle(), testSources())
	called := false
	err := s.WithSourceWrite(t.Context(), testPrincipal(), "web", func(_ context.Context, g *apiv1.Gaggle, _ SourceCredentialLoader) error {
		called = true
		g.Spec.InteractiveAccess.Actions = nil // The callback receives a copy.
		return nil
	})
	if err != nil || !called || len(registrar.values) != 0 {
		t.Fatal("receipt selection attempted credential resolution", called, err)
	}
	if err = s.Authorize(testPrincipal(), "web", "backlog.edit"); err != nil {
		t.Fatal("callback changed applied authority", err)
	}
}
