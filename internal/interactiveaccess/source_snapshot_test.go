package interactiveaccess

import (
	"context"
	"errors"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
)

func TestSourceSnapshotChecksEachActionTargetAndKeepsPrivateConfiguration(t *testing.T) {
	t.Setenv("HUMAN_ISSUES_TOKEN", "human-issues")
	t.Setenv("HUMAN_CODE_TOKEN", "human-code")
	g := testGaggle()
	s, _ := testService(t, g, testSources())
	err := s.WithSourceSnapshot(context.Background(), testPrincipal(), g.Name, func(ctx context.Context, copy *apiv1.Gaggle, load SnapshotCredentialLoader) error {
		copy.Spec.Backlog.Project = "foreign/project"
		credential, err := load(ctx, "backlog.read", Target{Kind: "backlog"})
		if err != nil || credential.Value != "human-issues" {
			t.Fatal("callback modified applied target", err)
		}
		if _, err = load(ctx, "repository.read", Target{Kind: "repository", Repository: repositoryIdentity(g.Spec.Project)}); err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			action apiv1.InteractiveAction
			target Target
		}{
			{"backlog.edit", Target{Kind: "backlog"}},
			{"backlog.read", Target{Kind: "repository", Repository: repositoryIdentity(g.Spec.Project)}},
			{"repository.read", Target{Kind: "repository", Repository: apiv1.InteractiveRepositoryIdentity{Provider: "github", Owner: "foreign", Name: "repo"}}},
		} {
			if _, err := load(ctx, tc.action, tc.target); err == nil {
				t.Fatal("source action widened", tc)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	g.Spec.InteractiveAccess.Actions = []apiv1.InteractiveAction{"repository.read"}
	if err = s.Apply([]apiv1.Gaggle{g}, nil); err != nil {
		t.Fatal(err)
	}
	err = s.WithSourceSnapshot(context.Background(), testPrincipal(), g.Name, func(ctx context.Context, _ *apiv1.Gaggle, load SnapshotCredentialLoader) error {
		if _, err := load(ctx, "backlog.read", Target{Kind: "backlog"}); !errors.Is(err, ErrDenied) {
			t.Fatal("undeclared action admitted", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
func TestSourceSnapshotRejectsTypedMachineAuthorityAndBoundsPolicyLockWait(t *testing.T) {
	s, _ := testService(t, testGaggle(), testSources())
	called := false
	callback := func(context.Context, *apiv1.Gaggle, SnapshotCredentialLoader) error { called = true; return nil }
	for _, mutate := range []func(*httpapi.Principal){
		func(p *httpapi.Principal) { p.GeneratedChild = true },
		func(p *httpapi.Principal) { p.WorkflowParent = true },
		func(p *httpapi.Principal) { p.ChildWorkflow = &httpapi.ChildWorkflowPrincipal{} },
		func(p *httpapi.Principal) { p.Scopes = []string{"read"} },
		func(p *httpapi.Principal) { p.Issuer = httpapi.PodPrincipalIssuer },
	} {
		p := testPrincipal()
		mutate(&p)
		if err := s.WithSourceSnapshot(context.Background(), p, "web", callback); !errors.Is(err, ErrDenied) {
			t.Fatal("machine reached source snapshot", err)
		}
	}
	if called {
		t.Fatal("denied snapshot invoked callback")
	}
	s.mu.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.WithSourceSnapshot(ctx, testPrincipal(), "web", callback) }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		s.mu.Unlock()
		t.Fatal("source snapshot exceeded bounded lock wait")
	}
	s.mu.Unlock()
	if called {
		t.Fatal("cancelled lock wait invoked callback")
	}
}
