package interactiveaccess

import (
	"context"
	"errors"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
)

func TestSourceReadBindsCurrentCopyAndExactCredential(t *testing.T) {
	t.Setenv("HUMAN_ISSUES_TOKEN", "human-issues")
	t.Setenv("HUMAN_CODE_TOKEN", "human-code")
	g := testGaggle()
	g.Spec.Workbench = &apiv1.GaggleWorkbench{SchemaVersion: "sources/v1", Sources: []apiv1.WorkbenchSource{{Name: "items", Kind: "backlog"}}}
	s, _ := testService(t, g, testSources())
	err := s.WithSourceRead(context.Background(), testPrincipal(), "web", "backlog.read", func(ctx context.Context, copy *apiv1.Gaggle, load SourceCredentialLoader) error {
		copy.Spec.Backlog.Project = "acme/other"
		copy.Spec.Workbench.Sources[0].Name = "changed"
		cred, err := load(ctx, Target{Kind: "backlog"})
		if err != nil {
			return err
		}
		if cred.Value != "human-issues" {
			t.Fatal("wrong source credential")
		}
		if _, err = load(ctx, Target{Kind: "repository", Repository: repositoryIdentity(g.Spec.Project)}); !errors.Is(err, ErrDenied) {
			t.Fatal("read action widened target", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	err = s.WithSourceView(context.Background(), testPrincipal(), "web", func(_ context.Context, copy *apiv1.Gaggle) error {
		if copy.Spec.Backlog.Project != "acme/issues" || copy.Spec.Workbench.Sources[0].Name != "items" {
			t.Fatal("callback changed live source policy")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSourceReadAndViewRejectForgedOrMissingAuthority(t *testing.T) {
	s, _ := testService(t, testGaggle(), testSources())
	called := false
	use := func(context.Context, *apiv1.Gaggle, SourceCredentialLoader) error { called = true; return nil }
	if err := s.WithSourceRead(context.Background(), testPrincipal(), "web", "backlog.edit", use); !errors.Is(err, ErrDenied) {
		t.Fatal(err)
	}
	for _, p := range []httpapi.Principal{{}, {Subject: "alice", Issuer: httpapi.PodPrincipalIssuer, Roles: []httpapi.Role{httpapi.RoleAdmin}}} {
		if err := s.WithSourceRead(context.Background(), p, "web", "backlog.read", use); !errors.Is(err, ErrDenied) {
			t.Fatal(err)
		}
		if err := s.WithSourceView(context.Background(), p, "web", func(context.Context, *apiv1.Gaggle) error { called = true; return nil }); !errors.Is(err, ErrDenied) {
			t.Fatal(err)
		}
	}
	if called {
		t.Fatal("unauthorized callback invoked")
	}
	g := testGaggle()
	g.Spec.InteractiveAccess = nil
	if err := s.Apply([]apiv1.Gaggle{g}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.WithSourceRead(context.Background(), testPrincipal(), "web", "backlog.read", use); !errors.Is(err, ErrDenied) {
		t.Fatal(err)
	}
}

func TestSessionSourceScopeCannotRetargetWorkbench(t *testing.T) {
	g := testGaggle()
	g.Spec.InteractiveAccess.Actions = append(g.Spec.InteractiveAccess.Actions, "session.message")
	g.Spec.Workbench = &apiv1.GaggleWorkbench{SchemaVersion: "sources/v1", Sources: []apiv1.WorkbenchSource{{Name: "items", Kind: "backlog"}}}
	s, _ := testService(t, g, testSources())
	lease, err := s.BeginSessionExecution(context.Background(), testPrincipal(), "web")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if err = lease.RequireWorkbenchSources(&g); err != nil {
		t.Fatal(err)
	}
	altered := g.DeepCopy()
	altered.Spec.Workbench.Sources[0].Name = "retargeted"
	if err = lease.RequireWorkbenchSources(altered); !errors.Is(err, ErrDenied) {
		t.Fatal(err)
	}
	altered = g.DeepCopy()
	altered.Spec.Backlog.Project = "acme/foreign"
	if err = lease.RequireWorkbenchSources(altered); !errors.Is(err, ErrDenied) {
		t.Fatal(err)
	}
	lease.cancel()
	if err = lease.RequireWorkbenchSources(&g); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
