package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/credentials"
)

func TestPreflightCredentialRefsFailsClosedOnMissingEnvToken(t *testing.T) {
	os.Unsetenv("GOOBERS_TEST_CMD_PREFLIGHT_MISSING")
	resolver, err := credentials.NewResolver([]credentials.TokenRef{
		{Name: "octo/repo", Env: "GOOBERS_TEST_CMD_PREFLIGHT_MISSING"},
	})
	if err != nil {
		t.Fatalf("build resolver: %v", err)
	}

	err = preflightCredentialRefs(resolver)
	if err == nil {
		t.Fatal("preflightCredentialRefs() = nil, want error")
	}
	if !errors.Is(err, credentials.ErrPreflight) {
		t.Fatalf("errors.Is(err, credentials.ErrPreflight) = false for %v", err)
	}
	if !strings.Contains(err.Error(), "GOOBERS_TEST_CMD_PREFLIGHT_MISSING") {
		t.Errorf("error does not name the unset variable: %q", err.Error())
	}
}

func TestPreflightCredentialRefsPassesWhenTokenPresent(t *testing.T) {
	t.Setenv("GOOBERS_TEST_CMD_PREFLIGHT_SET", "token-value")
	resolver, err := credentials.NewResolver([]credentials.TokenRef{
		{Name: "octo/repo", Env: "GOOBERS_TEST_CMD_PREFLIGHT_SET"},
	})
	if err != nil {
		t.Fatalf("build resolver: %v", err)
	}
	if err := preflightCredentialRefs(resolver); err != nil {
		t.Fatalf("preflightCredentialRefs() = %v, want nil", err)
	}
}

// A Resolver from another source (a stub in a test, a future remote resolver)
// cannot enumerate refs; startup must proceed rather than fail closed on it.
func TestPreflightCredentialRefsSkipsNonEnumerableResolver(t *testing.T) {
	if err := preflightCredentialRefs(stubResolver{}); err != nil {
		t.Fatalf("preflightCredentialRefs() = %v, want nil", err)
	}
}

type stubResolver struct{}

func (stubResolver) Resolve(context.Context, string) (string, error) { return "value", nil }
