package interactiveaccess

import (
	"context"
	"errors"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
)

func TestRepositorySourceCallbackBindsAuthorityWithoutMinting(t *testing.T) {
	g := testGaggle()
	s, registrar := testService(t, g, testSources())
	target := Target{Kind: "repository", Repository: repositoryIdentity(g.Spec.Project)}
	selected := func(copy *apiv1.Gaggle) (Target, error) { copy.Spec.Project.Name = "tampered-copy"; return target, nil }
	called := false
	err := s.WithRepositorySource(t.Context(), testPrincipal(), g.Name, "source.proposeChange", selected, func(context.Context, RepositoryCredentialLoader) error { called = true; return nil })
	if err != nil || !called || len(registrar.values) != 0 {
		t.Fatal(called, err, registrar.values)
	}
	for _, mode := range []string{"child", "generated", "parent", "scoped", "viewer", "foreign", "wrong action"} {
		p := testPrincipal()
		action := apiv1.InteractiveAction("source.proposeChange")
		selectedTarget := target
		switch mode {
		case "child":
			p.ChildWorkflow = &httpapi.ChildWorkflowPrincipal{}
		case "generated":
			p.GeneratedChild = true
		case "parent":
			p.WorkflowParent = true
		case "scoped":
			p.Scopes = []string{"x"}
		case "viewer":
			p.Roles = []httpapi.Role{httpapi.RoleView}
		case "foreign":
			selectedTarget.Repository.Name = "foreign"
		case "wrong action":
			action = "backlog.edit"
		}
		called = false
		err = s.WithRepositorySource(t.Context(), p, g.Name, action, func(*apiv1.Gaggle) (Target, error) { return selectedTarget, nil }, func(context.Context, RepositoryCredentialLoader) error { called = true; return nil })
		if err == nil || called {
			t.Fatal(mode, called, err)
		}
	}
}
func TestRepositorySourceCallbackHoldsAppliedPolicyUntilJoined(t *testing.T) {
	g := testGaggle()
	s, _ := testService(t, g, testSources())
	started, release, publishing := make(chan struct{}), make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- s.WithRepositorySource(t.Context(), testPrincipal(), g.Name, "source.proposeChange", func(*apiv1.Gaggle) (Target, error) {
			return Target{Kind: "repository", Repository: repositoryIdentity(g.Spec.Project)}, nil
		}, func(ctx context.Context, _ RepositoryCredentialLoader) error {
			close(started)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	<-started
	reloaded := make(chan error, 1)
	changed := g.DeepCopy()
	changed.Spec.InteractiveAccess.Actions = []apiv1.InteractiveAction{"repository.read"}
	go func() { reloaded <- s.Apply([]apiv1.Gaggle{*changed}, func() error { close(publishing); return nil }) }()
	select {
	case <-publishing:
		t.Fatal("policy changed during provider callback")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-reloaded; err != nil {
		t.Fatal(err)
	}
	called := false
	err := s.WithRepositorySource(t.Context(), testPrincipal(), g.Name, "source.proposeChange", func(*apiv1.Gaggle) (Target, error) { called = true; return Target{}, nil }, func(context.Context, RepositoryCredentialLoader) error { return nil })
	if !errors.Is(err, ErrDenied) || called {
		t.Fatal("revocation ignored", called, err)
	}
}
