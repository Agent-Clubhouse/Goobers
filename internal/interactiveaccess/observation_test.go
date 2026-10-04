package interactiveaccess

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
)

func TestRunObservationRequiresBothActionsAndExactHumanTarget(t *testing.T) {
	t.Setenv("HUMAN_CODE_TOKEN", "observation-code-token")
	for _, name := range []string{"allowed", "no intervention", "no repository read", "viewer", "other gaggle", "foreign repository", "missing source", "cancelled"} {
		t.Run(name, func(t *testing.T) {
			gaggle := testGaggle()
			principal := testPrincipal()
			target := Target{Kind: "repository", Repository: repositoryIdentity(gaggle.Spec.Project)}
			scope := gaggle.Name
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch name {
			case "no intervention":
				gaggle.Spec.InteractiveAccess.Actions = slices.DeleteFunc(gaggle.Spec.InteractiveAccess.Actions, func(action apiv1.InteractiveAction) bool { return action == "run.intervene" })
			case "no repository read":
				gaggle.Spec.InteractiveAccess.Actions = slices.DeleteFunc(gaggle.Spec.InteractiveAccess.Actions, func(action apiv1.InteractiveAction) bool { return action == "repository.read" })
			case "viewer":
				principal.Roles = []httpapi.Role{httpapi.RoleView}
			case "other gaggle":
				scope = "other"
			case "foreign repository":
				target.Repository.Owner = "other"
			case "missing source":
				gaggle.Spec.InteractiveAccess.Credentials.Repositories = nil
			case "cancelled":
				cancel()
			}
			service, registrar := testService(t, gaggle, testSources())
			called := false
			err := service.WithRunObservationCredential(ctx, principal, scope, target, func(_ context.Context, credential Credential) error {
				called = true
				if credential.Value != "observation-code-token" || !slices.Contains(registrar.values, credential.Value) {
					t.Fatal("observation did not use registered repository credential")
				}
				return nil
			})
			if name == "allowed" {
				if err != nil || !called {
					t.Fatal(err, called)
				}
			} else if err == nil || called || len(registrar.values) != 0 {
				t.Fatal("refused observation exposed a credential", err, called)
			}
		})
	}
}

func TestRunObservationFencesPolicyReloadThroughProviderRead(t *testing.T) {
	t.Setenv("HUMAN_CODE_TOKEN", "observation-code-token")
	gaggle := testGaggle()
	service, _ := testService(t, gaggle, testSources())
	target := Target{Kind: "repository", Repository: repositoryIdentity(gaggle.Spec.Project)}
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- service.WithRunObservationCredential(t.Context(), testPrincipal(), "web", target, func(context.Context, Credential) error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	gaggle.Spec.InteractiveAccess.Actions = []apiv1.InteractiveAction{"repository.read"}
	applying, published := make(chan struct{}), make(chan struct{})
	applied := make(chan error, 1)
	go func() {
		close(applying)
		applied <- service.Apply([]apiv1.Gaggle{gaggle}, func() error { close(published); return nil })
	}()
	<-applying
	select {
	case <-published:
		t.Error("policy published during an authorized observation")
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-applied; err != nil {
		t.Fatal(err)
	}
	if err := service.WithRunObservationCredential(t.Context(), testPrincipal(), "web", target, func(context.Context, Credential) error { t.Error("revoked operator reached observer"); return nil }); !errors.Is(err, ErrDenied) {
		t.Fatal(err)
	}
}
