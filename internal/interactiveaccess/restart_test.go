package interactiveaccess

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestRestartSourcesUseDistinctConfiguredIdentitiesAndFenceReload(t *testing.T) {
	t.Setenv("HUMAN_CODE_TOKEN", "code-secret")
	t.Setenv("HUMAN_ISSUES_TOKEN", "backlog-secret")
	g := testGaggle()
	g.Spec.InteractiveAccess.Actions = append(g.Spec.InteractiveAccess.Actions, "run.restartStage")
	service, registrar := testService(t, g, testSources())
	repo := repositoryIdentity(g.Spec.Project)
	entered, release := make(chan struct{}), make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		finished <- service.WithRestartSources(context.Background(), testPrincipal(), "web", RestartSourceRequest{Repository: &repo, Backlog: true}, func(_ context.Context, sources RestartSources) error {
			if sources.Repository.Value != "code-secret" || sources.Backlog.Value != "backlog-secret" || sources.BacklogIdentity.Name != "issues" {
				t.Error("restart credentials crossed source scopes")
			}
			// Caller mutation must not change the applied permission policy.
			sources.Gaggle.Spec.InteractiveAccess.Actions = nil
			close(entered)
			<-release
			return nil
		})
	}()
	select {
	case <-entered:
	case err := <-finished:
		t.Fatalf("callback unavailable: %v", err)
	case <-time.After(time.Second):
		t.Fatal("source resolution stuck")
	}
	revoked := g.DeepCopy()
	revoked.Spec.InteractiveAccess.Actions = nil
	applied := make(chan error, 1)
	go func() { applied <- service.Apply([]apiv1.Gaggle{*revoked}, nil) }()
	select {
	case err := <-applied:
		t.Fatalf("policy changed during source acceptance: %v", err)
	case <-time.After(10 * time.Millisecond):
	}
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if err := <-applied; err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(registrar.values, "code-secret") || !slices.Contains(registrar.values, "backlog-secret") {
		t.Fatal("interactive secrets were not registered before use")
	}
	if err := service.WithRestartSources(t.Context(), testPrincipal(), "web", RestartSourceRequest{Repository: &repo, Backlog: true}, func(context.Context, RestartSources) error { t.Fatal("revoked restart invoked callback"); return nil }); !errors.Is(err, ErrDenied) {
		t.Fatalf("revocation: %v", err)
	}
}

func TestRestartSourcesNeverFallbackToCodeCredentialForBacklog(t *testing.T) {
	t.Setenv("HUMAN_CODE_TOKEN", "code-secret")
	t.Setenv("HUMAN_ISSUES_TOKEN", "backlog-secret")
	g := testGaggle()
	g.Spec.InteractiveAccess.Actions = append(g.Spec.InteractiveAccess.Actions, "run.restartStage")
	g.Spec.InteractiveAccess.Credentials.Backlog = "code"
	service, _ := testService(t, g, testSources())
	repo := repositoryIdentity(g.Spec.Project)
	err := service.WithRestartSources(t.Context(), testPrincipal(), "web", RestartSourceRequest{Repository: &repo, Backlog: true}, func(context.Context, RestartSources) error {
		t.Fatal("mismatched backlog credential accepted")
		return nil
	})
	if !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatalf("mismatched source: %v", err)
	}
	foreign := repo
	foreign.Name = "other"
	err = service.WithRestartSources(t.Context(), testPrincipal(), "web", RestartSourceRequest{Repository: &foreign}, func(context.Context, RestartSources) error { t.Fatal("foreign repository accepted"); return nil })
	if !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatalf("foreign source: %v", err)
	}
}

func TestRestartPermissionDoesNotImplySourceReadPermission(t *testing.T) {
	g := testGaggle()
	g.Spec.InteractiveAccess.Actions = []apiv1.InteractiveAction{"run.restartStage"}
	service, _ := testService(t, g, testSources())
	repo := repositoryIdentity(g.Spec.Project)
	if err := service.WithRestartSources(t.Context(), testPrincipal(), "web", RestartSourceRequest{Repository: &repo}, func(context.Context, RestartSources) error { t.Fatal("missing read action accepted"); return nil }); !errors.Is(err, ErrDenied) {
		t.Fatal(err)
	}
	called := false
	if err := service.WithRestartSources(t.Context(), testPrincipal(), "web", RestartSourceRequest{}, func(context.Context, RestartSources) error { called = true; return nil }); err != nil || !called {
		t.Fatalf("scratch: %v %v", called, err)
	}
}

func TestRestartReceiptAdmissionDoesNotRequireCredentialResolution(t *testing.T) {
	g := testGaggle()
	g.Spec.InteractiveAccess.Actions = []apiv1.InteractiveAction{"run.restartStage"}
	service, _ := testService(t, g, testSources())
	called := false
	if err := service.WithRestartAdmission(t.Context(), testPrincipal(), "web", func(context.Context, RestartSourceLoader) error { called = true; return nil }); err != nil || !called {
		t.Fatalf("receipt admission: %t %v", called, err)
	}
	if err := service.WithRestartAdmission(t.Context(), testPrincipal(), "web", func(ctx context.Context, load RestartSourceLoader) error {
		_, err := load(ctx, RestartSourceRequest{Backlog: true})
		return err
	}); !errors.Is(err, ErrDenied) {
		t.Fatalf("lazy resolution bypassed source permission: %v", err)
	}
}
