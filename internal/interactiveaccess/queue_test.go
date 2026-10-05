package interactiveaccess

import (
	"context"
	"errors"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
)

func TestQueueCancellationRequiresExplicitGrantAndHonorsLockBudget(t *testing.T) {
	g := testGaggle()
	g.Spec.InteractiveAccess.Actions = append(g.Spec.InteractiveAccess.Actions, "queue.cancel")
	s, _ := testService(t, g, testSources())
	called := 0
	use := func(context.Context) error { called++; return nil }
	if err := s.WithQueueCancellation(t.Context(), testPrincipal(), "web", use); err != nil || called != 1 {
		t.Fatal(err, called)
	}
	for _, p := range []httpapi.Principal{{}, {Issuer: "https://identity.example", Subject: "unlisted", Roles: []httpapi.Role{httpapi.RoleAdmin}}, {Issuer: "https://identity.example", Subject: "alice", Roles: []httpapi.Role{httpapi.RoleOperate}, Scopes: []string{"run"}}} {
		if err := s.WithQueueCancellation(t.Context(), p, "web", use); !errors.Is(err, ErrDenied) {
			t.Fatal(err)
		}
	}
	s.mu.Lock()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	err := s.WithQueueCancellation(ctx, testPrincipal(), "web", use)
	cancel()
	s.mu.Unlock()
	if !errors.Is(err, context.DeadlineExceeded) || called != 1 {
		t.Fatal(err, called)
	}
	g.Spec.InteractiveAccess.Actions = nil
	if err = s.Apply([]apiv1.Gaggle{g}, nil); err != nil {
		t.Fatal(err)
	}
	if err = s.WithQueueCancellation(t.Context(), testPrincipal(), "web", use); !errors.Is(err, ErrDenied) {
		t.Fatal(err)
	}
}
