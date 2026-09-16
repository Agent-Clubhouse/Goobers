package harness

import (
	"context"
	"errors"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/journal"
)

func TestOwnedBranchAuthorReceivesNoRepositoryCredentials(t *testing.T) {
	called := false
	adapter := &FakeAdapter{Act: func(ctx context.Context, req RunRequest) error {
		called = true
		for _, key := range []string{"repo:read", "repo:push", "contents:read"} {
			if _, err := req.Credentials.Token(ctx, key); !errors.Is(err, credentials.ErrUndeclaredCapability) {
				t.Errorf("%s was available to owned authoring: %v", key, err)
			}
		}
		return WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
	}}
	injector := testInjector(t, "OWNED_AUTHOR_TOKEN", "owned-author-fixture-token", noopRegistrar{})
	rec := &fakeRecorder{}
	exec, err := NewExecutor(adapter, injector, rec, rec, rec, journal.NewPatternScrubber(), "")
	if err != nil {
		t.Fatal(err)
	}
	env := testEnvelope(t.TempDir(), "repo:read", "repo:push", "contents:read")
	env.WorkspaceBranchBinding = &apiv1.WorkspaceBranchBinding{
		Repository: apiv1.RepositoryIdentity{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "base"},
		Ref:        "refs/heads/ns/workflow/run", StartingSHA: strings.Repeat("a", 40),
	}
	if _, err := exec.Invoke(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("authoring did not execute")
	}
}
